/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containerd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/benchmark/types"
	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/rs/xid"
)

func Supported() error {
	if err := shell.Supported(); err != nil {
		return err
	}
	return compose.Supported()
}

const defaultContainerdConfigPath = "/etc/containerd/config.toml"

// Runner runs benchmraks on containerd + Stargz Snapshotter.
type Runner struct {
	c    *compose.Compose
	sh   *shell.Shell
	repo string
}

func NewRunner(t *testing.T, repo string) *Runner {
	var (
		targetStage = "snapshotter-base"
		serviceName = "runner"
		pRoot       = testutil.GetProjectRoot(t)
	)
	contextDir, err := ioutil.TempDir("", "tmpcontext")
	if err != nil {
		t.Fatalf("failed to create temp context")
	}
	defer os.RemoveAll(contextDir)
	err = ioutil.WriteFile(filepath.Join(contextDir, "config.containerd.toml"), []byte(`
version = 2

[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
`), 0666)
	if err != nil {
		t.Fatalf("failed to write tmp containerd config file")
	}

	benchmarkImage, iDone, err := dexec.NewTempImage(pRoot, targetStage,
		dexec.WithTempImageStdio(testutil.TestingLogDest()),
		dexec.WithPatchContextDir(contextDir),
		dexec.WithPatchDockerfile(`
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    git clone https://github.com/google/go-containerregistry \
              \${GOPATH}/src/github.com/google/go-containerregistry && \
    cd \${GOPATH}/src/github.com/google/go-containerregistry && \
    git checkout 4b1985e5ea2104672636879e1694808f735fd214 && \
    GO111MODULE=on go get github.com/google/go-containerregistry/cmd/crane

COPY ./config.containerd.toml /etc/containerd/config.toml
`))
	if err != nil {
		t.Fatalf("failed to prepare temp testing image: %v", err)
	}
	defer iDone()
	c, err := compose.New(testutil.ApplyTextTemplate(t, `
version: "3.7"
services:
  {{.ServiceName}}:
    image: {{.BenchmarkImageName}}
    privileged: true
    init: true
    entrypoint: [ "sleep", "infinity" ]
    working_dir: /go/src/github.com/containerd/stargz-snapshotter
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "/dev/fuse:/dev/fuse"
    - "containerd-data:/var/lib/containerd:delegated"
    - "containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc:delegated"
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
`, struct {
		ServiceName        string
		BenchmarkImageName string
	}{
		ServiceName:        serviceName,
		BenchmarkImageName: benchmarkImage,
	}), compose.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to prepare compose: %v", err)
	}
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v", serviceName)
	}

	return &Runner{c, shell.New(de, testutil.NewTestingReporter(t)), repo}
}

func (cr *Runner) Run(t *testing.T, name string, spec interface{}, m types.Mode) types.Result {
	monitor := rebootContainerd(t, cr.sh, m)
	if monitor == nil && (m == types.EStargz || m == types.EStargzNoopt) {
		t.Fatalf("no monitor found but stargz snapshotter is running")
	}
	res := types.Result{
		Mode:  m.String(),
		Image: name,
	}
	img := formatImageName(t, m, cr.repo, name)
	res.ElapsedPullMilliSec = cr.measureCmd(t, pullCmd(t, m, img), "", "").Milliseconds()
	var (
		cid        string
		createTime int64
		runTime    int64
	)
	switch v := spec.(type) {
	case types.BenchEchoHello:
		cid, createTime, runTime = cr.runBenchEchoHello(t, img, m)
	case types.BenchCmdArg:
		cid, createTime, runTime = cr.runBenchCmdArg(t, img, v, m)
	case types.BenchCmdArgWait:
		cid, createTime, runTime = cr.runBenchCmdArgWait(t, img, v, m)
	case types.BenchCmdStdin:
		cid, createTime, runTime = cr.runBenchCmdStdin(t, img, v, m)
	default:
		t.Fatalf("unknown type of spec: %T", v)
	}
	if monitor != nil {
		monitor.CheckAllRemoteSnapshots(t)
	}
	cr.sh.XLog("ctr-remote", "t", "kill", "-s", "9", cid) // cleanup
	res.ElapsedCreateMilliSec = createTime
	res.ElapsedRunMilliSec = runTime
	return res
}

func (cr *Runner) Cleanup() error {
	return cr.c.Cleanup()
}

func (cr *Runner) runBenchEchoHello(t *testing.T, img string, m types.Mode) (string, int64, int64) {
	cid := "benchmark-" + xid.New().String()
	create := cr.measureCmd(t, fmt.Sprintf(
		"ctr-remote c create --net-host --snapshotter %q -- %q %q echo hello",
		snapshotter(t, m), img, cid), "", "").Milliseconds()
	run := cr.measureCmd(t,
		fmt.Sprintf("ctr-remote t start %q", cid), "", "").Milliseconds()
	return cid, create, run
}

func (cr *Runner) runBenchCmdArg(t *testing.T, img string, spec types.BenchCmdArg, m types.Mode) (string, int64, int64) {
	cid := "benchmark-" + xid.New().String()
	createCmdStr := fmt.Sprintf("ctr-remote c create --net-host --snapshotter %q -- %q %q",
		snapshotter(t, m), img, cid)
	for _, c := range spec.Args {
		createCmdStr += fmt.Sprintf(" %q", c)
	}
	create := cr.measureCmd(t, createCmdStr, "", "").Milliseconds()
	run := cr.measureCmd(t, "ctr-remote t start "+cid, "", "").Milliseconds()
	return cid, create, run
}

func (cr *Runner) runBenchCmdArgWait(t *testing.T, img string, spec types.BenchCmdArgWait, m types.Mode) (string, int64, int64) {
	cid := "benchmark-" + xid.New().String()
	createCmdStr := fmt.Sprintf("ctr-remote c create --net-host --snapshotter %q ", snapshotter(t, m))
	for _, e := range spec.Env {
		createCmdStr += fmt.Sprintf("--env %q ", e)
	}
	createCmdStr += fmt.Sprintf(" -- %q %q", img, cid)
	for _, c := range spec.Args {
		createCmdStr += fmt.Sprintf(" %q", c)
	}
	create := cr.measureCmd(t, createCmdStr, "", "").Milliseconds()
	run := cr.measureCmd(t, "ctr-remote t start "+cid, spec.WaitLine, "").Milliseconds()
	return cid, create, run
}

func (cr *Runner) runBenchCmdStdin(t *testing.T, img string, spec types.BenchCmdStdin, m types.Mode) (string, int64, int64) {
	cid := "benchmark-" + xid.New().String()
	createCmdStr := fmt.Sprintf("ctr-remote c create --net-host --snapshotter %q ", snapshotter(t, m))
	for _, m := range spec.Mounts {
		srcInSh := "/mountsource" + xid.New().String()
		if err := testutil.CopyInDir(cr.sh, m.Src, srcInSh); err != nil {
			t.Fatalf("failed to copy mount dir: %v", err)
		}
		createCmdStr += fmt.Sprintf("--mount type=bind,src=%s,dst=%s,options=rbind ",
			srcInSh, m.Dst)
	}
	createCmdStr += fmt.Sprintf(" -- %q %q ", img, cid)
	for _, c := range spec.Args {
		createCmdStr += fmt.Sprintf(" %q", c)
	}
	create := cr.measureCmd(t, createCmdStr, "", "").Milliseconds()
	run := cr.measureCmd(t, "ctr-remote t start "+cid, "", spec.Stdin).Milliseconds()
	return cid, create, run
}

func (cr *Runner) measureCmd(t *testing.T, cmdStr string, endStr string, stdinStr string) time.Duration {
	testutil.TestingL.Printf("Measuring %v\n", cmdStr)
	id := xid.New().String()
	startStr := "CMDSTART-" + id
	if endStr == "" {
		endStr = "CMDEND-" + id
	}
	// This command first prints startStr then execute the specified command and
	// finally prints endStr. If a stdout line by the executed command's output
	// contains endStr, that line will be omitted. This makes sure that endStr
	// only appers at the end of the command execution.
	cmd := cr.sh.Command("/bin/bash", "-euo", "pipefail", "-c",
		fmt.Sprintf("echo %q && %s | ( grep -v %q || true ) && echo %q",
			startStr, cmdStr, endStr, endStr),
	)
	if stdinStr != "" {
		cmd.Stdin = bytes.NewReader([]byte(stdinStr))
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout = outW
	cmd.Stderr = errW
	m := newMeasure(t, startStr, endStr, outR, errR)
	elapsedCh := make(chan time.Duration)
	errCh := make(chan error)
	go func() {
		if err := cmd.Run(); err != nil {
			outW.CloseWithError(err)
			errW.CloseWithError(err)
			if !m.isDone() {
				errCh <- fmt.Errorf("command failed before measuring ends: %v", err)
			}
			return
		}
		outW.Close()
		errW.Close()
	}()
	go func() {
		e, err := m.wait()
		if err != nil {
			errCh <- err
			return
		}
		elapsedCh <- e
	}()
	var e time.Duration
	select {
	case e = <-elapsedCh:
	case err := <-errCh:
		t.Fatalf("failed to measure: %v", err)
	}
	return e
}

type measure struct {
	start *time.Time
	end   *time.Time

	mu         sync.Mutex
	finishCond *sync.Cond
	errCh      chan error
}

func newMeasure(t *testing.T, startStr, endStr string, reads ...io.Reader) *measure {
	m := &measure{
		finishCond: sync.NewCond(&sync.Mutex{}),
		errCh:      make(chan error),
	}
	for i, r := range reads {
		i, r := i, r
		go func() {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				m.mu.Lock()
				if m.end != nil {
					m.mu.Unlock()
					m.errCh <- fmt.Errorf("scanning already ended measure")
					return
				}
				m.mu.Unlock()
				l := scanner.Text()
				testutil.TestingL.Printf("out[%d]: %s\n", i, l)
				if strings.Contains(l, startStr) {
					m.mu.Lock()
					if m.start != nil {
						m.errCh <- fmt.Errorf("starting already started measure")
						return
					}
					now := time.Now()
					m.start = &now
					m.mu.Unlock()
					testutil.TestingL.Println("starting measure", m.start)
				}
				if strings.Contains(l, endStr) {
					m.mu.Lock()
					now := time.Now()
					m.end = &now
					m.mu.Unlock()
					m.finishCond.Broadcast()
					testutil.TestingL.Println("ending measure", m.end)
					return
				}
			}
			m.mu.Lock()
			if m.start == nil || m.end == nil {
				m.mu.Unlock()
				m.errCh <- fmt.Errorf("unexpectedly reached EOF: started: %v, ended: %v",
					m.start, m.end)
			}
			m.mu.Unlock()
		}()
	}
	return m
}

func (m *measure) wait() (time.Duration, error) {
	elapsedCh := make(chan time.Duration)
	go func() {
		m.finishCond.L.Lock()
		m.mu.Lock()
		ended := m.end != nil
		m.mu.Unlock()
		if !ended {
			m.finishCond.Wait()
		}
		m.finishCond.L.Unlock()
		elapsedCh <- m.end.Sub(*m.start)
	}()
	var e time.Duration
	select {
	case e = <-elapsedCh:
	case err := <-m.errCh:
		return 0, err
	}
	return e, nil
}

func (m *measure) isDone() bool {
	m.mu.Lock()
	done := m.end != nil
	m.mu.Unlock()
	return done
}

func formatImageName(t *testing.T, m types.Mode, repo, name string) string {
	switch m {
	case types.Legacy:
		return repo + "/" + name + "-org"
	case types.EStargz:
		return repo + "/" + name + "-esgz"
	case types.EStargzNoopt:
		return repo + "/" + name + "-esgz-noopt"
	}
	t.Fatalf("unknown mode %v", m)
	return ""
}

func pullCmd(t *testing.T, m types.Mode, img string) string {
	switch m {
	case types.Legacy:
		return fmt.Sprintf("ctr-remote i pull %q", img)
	case types.EStargz:
		return fmt.Sprintf("ctr-remote i rpull %q", img)
	case types.EStargzNoopt:
		return fmt.Sprintf("ctr-remote i rpull %q", img)
	}
	t.Fatalf("unknown mode %v", m)
	return ""
}

func snapshotter(t *testing.T, m types.Mode) string {
	switch m {
	case types.Legacy:
		return "overlayfs"
	case types.EStargz:
		return "stargz"
	case types.EStargzNoopt:
		return "stargz"
	}
	t.Fatalf("unknown mode %v", m)
	return ""
}

func rebootContainerd(t *testing.T, sh *shell.Shell, m types.Mode) *testutil.RemoteSnapshotMonitor {
	var (
		containerdRoot    = "/var/lib/containerd/"
		containerdStatus  = "/run/containerd/"
		snapshotterSocket = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
		snapshotterRoot   = "/var/lib/containerd-stargz-grpc/"
	)
	noprefetch := true
	if m == types.EStargz {
		noprefetch = false // DO prefetch for eStargz
	}
	runSnapshotter := false
	if m == types.EStargz || m == types.EStargzNoopt {
		runSnapshotter = true
	}

	// cleanup directories
	testutil.KillMatchingProcess(sh, "containerd")
	testutil.KillMatchingProcess(sh, "containerd-stargz-grpc")
	removeUnder(sh, containerdRoot)
	if isFileExists(sh, snapshotterSocket) {
		sh.X("rm", snapshotterSocket)
	}
	if snDir := filepath.Join(snapshotterRoot, "/snapshotter/snapshots"); isDirExists(sh, snDir) {
		sh.X("find", snDir, "-maxdepth", "1", "-mindepth", "1", "-type", "d",
			"-exec", "umount", "{}/fs", ";")
	}
	if snDir := filepath.Join(containerdStatus, "io.containerd.runtime.v2.task/default"); isDirExists(sh, snDir) {
		sh.X("find", snDir, "-maxdepth", "1", "-mindepth", "1", "-type", "d",
			"-exec", "umount", "{}/rootfs", ";")
	}
	if isDirExists(sh, containerdStatus) {
		removeUnder(sh, containerdStatus)
	}
	removeUnder(sh, snapshotterRoot)

	// run containerd and snapshotter
	var monitor *testutil.RemoteSnapshotMonitor
	if runSnapshotter {
		configPath := strings.TrimSpace(string(sh.O("mktemp")))
		if err := testutil.WriteFileContents(sh, configPath, []byte(fmt.Sprintf("noprefetch = %v", noprefetch)), 0600); err != nil {
			t.Fatalf("failed to write snapshotter config file: %v", err)
		}
		outR, errR, err := sh.R("containerd-stargz-grpc", "--log-level", "debug",
			"--address", snapshotterSocket, "--config", configPath)
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		monitor = testutil.NewRemoteSnapshotMonitor(testutil.NewTestingReporter(t), outR, errR)
	} else {
		testutil.TestingL.Println("DO NOT RUN remote snapshotter")
	}

	sh.Gox("containerd", "--log-level", "debug", "--config", defaultContainerdConfigPath)
	sh.Retry(100, "ctr-remote", "version")

	// make sure containerd and containerd-stargz-grpc are up-and-running
	if runSnapshotter {
		sh.Retry(100, "ctr-remote", "snapshots", "--snapshotter", "stargz",
			"prepare", "connectiontest-dummy-"+xid.New().String(), "")
	}

	return monitor
}

func removeUnder(sh *shell.Shell, dir string) {
	sh.X("find", dir+"/.", "!", "-name", ".", "-prune", "-exec", "rm", "-rf", "{}", "+")
}

func isFileExists(sh *shell.Shell, file string) bool {
	return sh.Command("test", "-f", file).Run() == nil
}

func isDirExists(sh *shell.Shell, dir string) bool {
	return sh.Command("test", "-d", dir).Run() == nil
}
