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

package testutil

// This file contains some utilities that supports to manipulate dockershell.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
)

// TestingReporter is an implementation of dockershell.Reporter backed by testing.T and TestingL.
type TestingReporter struct {
	t *testing.T
}

// NewTestingReporter returns a new TestingReporter instance for the specified testing.T.
func NewTestingReporter(t *testing.T) *TestingReporter {
	return &TestingReporter{t}
}

// Errorf prints the provided message to TestingL and stops the test using testing.T.Fatalf.
func (r *TestingReporter) Errorf(format string, v ...interface{}) {
	TestingL.Printf(format, v...)
	r.t.Fatalf(format, v...)
}

// Logf prints the provided message to TestingL testing.T.
func (r *TestingReporter) Logf(format string, v ...interface{}) {
	TestingL.Printf(format, v...)
	r.t.Logf(format, v...)
}

// Stdout returns the writer to TestingL as stdout. This enables to print command logs realtime.
func (r *TestingReporter) Stdout() io.Writer {
	return TestingL.Writer()
}

// Stderr returns the writer to TestingL as stderr. This enables to print command logs realtime.
func (r *TestingReporter) Stderr() io.Writer {
	return TestingL.Writer()
}

// RemoteSnapshotMonitor scans log of stargz snapshotter and provides the way to check
// if all snapshots are prepared as remote snpashots.
type RemoteSnapshotMonitor struct {
	remote uint64
	local  uint64
}

// NewRemoteSnapshotMonitor creates a new instance of RemoteSnapshotMonitor that scans logs streamed
// from the specified io.Reader.
func NewRemoteSnapshotMonitor(r shell.Reporter, stdout, stderr io.Reader) *RemoteSnapshotMonitor {
	m := &RemoteSnapshotMonitor{}
	go m.ScanLog(io.TeeReader(stdout, r.Stdout()))
	go m.ScanLog(io.TeeReader(stderr, r.Stderr()))
	return m
}

type RemoteSnapshotPreparedLogLine struct {
	RemoteSnapshotPrepared string `json:"remote-snapshot-prepared"`
}

// ScanLog scans the log streamed from the specified io.Reader.
func (m *RemoteSnapshotMonitor) ScanLog(inputR io.Reader) {
	scanner := bufio.NewScanner(inputR)
	var logline RemoteSnapshotPreparedLogLine
	for scanner.Scan() {
		rawL := scanner.Text()
		if i := strings.Index(rawL, "{"); i > 0 {
			rawL = rawL[i:] // trim garbage chars; expects "{...}"-styled JSON log
		}
		if err := json.Unmarshal([]byte(rawL), &logline); err == nil {
			if logline.RemoteSnapshotPrepared == "true" {
				atomic.AddUint64(&m.remote, 1)
			} else if logline.RemoteSnapshotPrepared == "false" {
				atomic.AddUint64(&m.local, 1)
			}
		}
	}
}

// CheckAllRemoteSnapshots checks if the scanned log reports that all snapshots are prepared
// as remote snapshots.
func (m *RemoteSnapshotMonitor) CheckAllRemoteSnapshots(t *testing.T) {
	remote := atomic.LoadUint64(&m.remote)
	local := atomic.LoadUint64(&m.local)
	result := fmt.Sprintf("(local:%d,remote:%d)", local, remote)
	if local > 0 {
		t.Fatalf("some local snapshots creation have been reported %v", result)
	} else if remote > 0 {
		t.Logf("all layers have been reported as remote snapshots %v", result)
		return
	} else {
		t.Fatalf("no log for checking remote snapshot was provided; Is the log-level = debug?")
	}
}

// TempDir creates a temporary directory in the specified execution environment.
func TempDir(sh *shell.Shell) (string, error) {
	out, err := sh.Command("mktemp", "-d").Output()
	if err != nil {
		return "", fmt.Errorf("failed to run mktemp: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func writeFileFromReader(sh *shell.Shell, name string, content io.Reader, mode uint32) error {
	if err := sh.Command("mkdir", "-p", filepath.Dir(name)).Run(); err != nil {
		return err
	}
	cmd := sh.Command("/bin/sh", "-c", fmt.Sprintf("cat > %s && chmod %#o %s", name, mode, name))
	cmd.Stdin = content
	return cmd.Run()
}

// WriteFileContents creates a file at the specified location in the specified execution environment
// and writes the specified contents to that file.
func WriteFileContents(sh *shell.Shell, name string, content []byte, mode uint32) error {
	return writeFileFromReader(sh, name, bytes.NewReader(content), mode)
}

// CopyInDir copies a directory into the specified location in the specified execution environment.
func CopyInDir(sh *shell.Shell, from, to string) error {
	if !filepath.IsAbs(from) || !filepath.IsAbs(to) {
		return fmt.Errorf("path %v and %v must be absolute path", from, to)
	}

	pr, pw := io.Pipe()
	cmdFrom := exec.Command("tar", "-zcf", "-", "-C", from, ".")
	cmdFrom.Stdout = pw
	var eg errgroup.Group
	eg.Go(func() error {
		if err := cmdFrom.Run(); err != nil {
			pw.CloseWithError(err)
			return err
		}
		pw.Close()
		return nil
	})

	tmpTar := "/tmptar" + xid.New().String()
	if err := writeFileFromReader(sh, tmpTar, pr, 0755); err != nil {
		return errors.Wrapf(err, "writeFileFromReader")
	}
	if err := eg.Wait(); err != nil {
		return errors.Wrapf(err, "taring")
	}
	if err := sh.Command("mkdir", "-p", to).Run(); err != nil {
		return errors.Wrapf(err, "mkdir -p %v", to)
	}
	if err := sh.Command("tar", "zxf", tmpTar, "-C", to).Run(); err != nil {
		return errors.Wrapf(err, "tar zxf %v -C %v", tmpTar, to)
	}
	return sh.Command("rm", tmpTar).Run()
}

// KillMatchingProcess kills processes that "ps" line matches the specified pattern in the
// specified execution environment.
func KillMatchingProcess(sh *shell.Shell, psLinePattern string) error {
	data, err := sh.Command("ps", "auxww").Output()
	if err != nil {
		return fmt.Errorf("failed to run ps command : %v", err)
	}
	var targets []int
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		psline := scanner.Text()
		matched, err := regexp.Match(psLinePattern, []byte(psline))
		if err != nil {
			return err
		}
		if matched {
			es := strings.Fields(psline)
			if len(es) < 2 {
				continue
			}
			pid, err := strconv.ParseInt(es[1], 10, 32)
			if err != nil {
				continue
			}
			targets = append(targets, int(pid))
		}
	}

	var allErr error
	for _, pid := range targets {
		if err := sh.Command("kill", "-9", fmt.Sprintf("%d", pid)).Run(); err != nil {
			multierror.Append(allErr, errors.Wrapf(err, "failed to kill %v", pid))
		}
	}
	return allErr
}
