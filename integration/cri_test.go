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

package integration

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/reference"
	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/pkg/errors"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const containerdSocketRef = "unix:///run/containerd/containerd.sock"

// TestCRI tests stargz snapshotter passes CRI integration tests provided by cri-tools
func TestCRI(t *testing.T) {
	t.Parallel()
	imagesList := testLegacyImage(t)
	testEStargzImage(t, imagesList)
}

func testLegacyImage(t *testing.T) []string {
	sh, _, _, done := newCRITestingNode(t)
	defer done()
	sh.
		Retry(100, "ctr", "version").
		X("runc", "--version").
		X("containerd", "--version").
		X("/go/bin/critest", "--runtime-endpoint="+containerdSocketRef)

	images := map[string]struct{}{}
	err := sh.ForEach(shell.C("journalctl", "-xu", "containerd"), func(l string) bool {
		if m := regexp.MustCompile(`PullImage \\"([^\\]*)\\"`).FindStringSubmatch(l); len(m) >= 2 {
			images[m[1]] = struct{}{}
		}
		if m := regexp.MustCompile(`SandboxImage:([^ ]*)`).FindStringSubmatch(l); len(m) >= 2 {
			images[m[1]] = struct{}{}
		}
		return true
	})
	if err != nil {
		t.Fatalf("failed to run journalctl: %v", err)
	}
	var imagesList []string
	for i := range images {
		imagesList = append(imagesList, i)
	}
	return imagesList
}

func testEStargzImage(t *testing.T, imagesList []string) {
	sh, shP, registryHost, done := newCRITestingNode(t)
	defer done()

	// Optimize and mirror target images and modify containerd config
	sources, dgsts := mirrorCRIImages(t, shP, registryHost, imagesList)
	for org, new := range dgsts {
		sh.X("find", "/go/src/github.com/kubernetes-sigs/cri-tools/pkg",
			"-type", "f", "-exec", "sed", "-i", "-e", "s|"+org+"|"+new+"|g", "{}", ";")
	}
	sh.X("/bin/bash", "-c", "cd /go/src/github.com/kubernetes-sigs/cri-tools && make && make install -e BINDIR=/go/bin")
	var containerdAddConfig string
	var snapshotterAddConfig string
	for _, s := range sources {
		// construct config
		containerdAddConfig += fmt.Sprintf(`
[plugins."io.containerd.grpc.v1.cri".registry.mirrors."%s"]
endpoint = ["http://%s"]
`, s, registryHost)
		if isTestingBuiltinSnapshotter() {
			containerdAddConfig += fmt.Sprintf(`
[[plugins."io.containerd.snapshotter.v1.stargz".resolver.host."%s".mirrors]]
host = "%s"
insecure = true
`, s, registryHost)
		} else {
			snapshotterAddConfig += fmt.Sprintf(`
[[resolver.host."%s".mirrors]]
host = "%s"
insecure = true
`, s, registryHost)
		}
	}
	if isTestingBuiltinSnapshotter() {
		// export JSON-formatted debug log for enabling to monitor
		// remote snapshot creation
		containerdAddConfig += `
[debug]
  format = "json"
  level = "debug"
`
	}
	appendFileContents(t, sh, defaultContainerdConfigPath, containerdAddConfig)
	appendFileContents(t, sh, defaultSnapshotterConfigPath, snapshotterAddConfig)
	if !isTestingBuiltinSnapshotter() {
		sh.X("systemctl", "restart", "stargz-snapshotter")
	}
	sh.X("systemctl", "restart", "containerd")

	// Run CRI test
	sh.
		Retry(100, "ctr", "version").
		X("runc", "--version").
		X("containerd", "--version").
		X("/go/bin/critest", "--runtime-endpoint="+containerdSocketRef)
	m := &testutil.RemoteSnapshotMonitor{}
	cmd := shell.C("journalctl", "-u", "stargz-snapshotter")
	if isTestingBuiltinSnapshotter() {
		cmd = shell.C("journalctl", "-u", "containerd")
	}
	stdout, stderr, err := sh.R(cmd...)
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	var wg sync.WaitGroup
	for _, r := range []io.Reader{stdout, stderr} {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ScanLog(r)
		}()
	}
	wg.Wait()
	m.CheckAllRemoteSnapshots(t)
}

func newCRITestingNode(t *testing.T) (*shell.Shell, *shell.Shell, string, func() error) {
	var (
		reporter           = testutil.NewTestingReporter(t)
		pRoot              = testutil.GetProjectRoot(t)
		criServiceName     = "cri_test"
		prepareServiceName = "prepare"
		registryHost       = "registry-" + xid.New().String() + ".test"
		registryHostPort   = registryHost + ":5000"
	)
	targetStage := ""
	if isTestingBuiltinSnapshotter() {
		targetStage = "kind-builtin-snapshotter"
	}
	testImageName, iDone, err := dexec.NewTempImage(pRoot, targetStage,
		dexec.WithTempImageStdio(testutil.TestingLogDest()),
		dexec.WithTempImageBuildArgs(getBuildArgsFromEnv(t)...),
		dexec.WithPatchDockerfile(testutil.ApplyTextTemplate(t, `
ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    apt install -y --no-install-recommends git make gcc build-essential jq && \
    curl https://dl.google.com/go/go1.15.6.linux-${TARGETARCH:-amd64}.tar.gz \
    | tar -C /usr/local -xz && \
    go get -u github.com/onsi/ginkgo/ginkgo && \
    git clone https://github.com/kubernetes-sigs/cri-tools \
              ${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    cd ${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    git checkout {{.CRIToolsVersion}} && \
    make && make install -e BINDIR=${GOPATH}/bin && \
    git clone -b v1.11.1 https://github.com/containerd/cri \
              ${GOPATH}/src/github.com/containerd/cri && \
    cd ${GOPATH}/src/github.com/containerd/cri && \
    NOSUDO=true ./hack/install/install-cni.sh && \
    NOSUDO=true ./hack/install/install-cni-config.sh && \
    systemctl disable kubelet
`, struct {
			CRIToolsVersion string
		}{
			CRIToolsVersion: testutil.CRIToolsVersion,
		})))
	if err != nil {
		t.Fatalf("failed to prepare temp testing image: %v", err)
	}
	defer iDone()
	c, err := compose.New(testutil.ApplyTextTemplate(t, `
version: "3.7"
services:
  {{.CRIServiceName}}:
    image: {{.TestImageName}}
    privileged: true
    environment:
    - NO_PROXY=127.0.0.1,localhost
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - /dev/fuse:/dev/fuse
    - "containerd-data:/var/lib/containerd"
    - "containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
  {{.PrepareServiceName}}:
    build:
      context: {{.ImageContextDir}}
      target: snapshotter-base
    privileged: true
    init: true
    entrypoint: [ "sleep", "infinity" ]
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "prepare-containerd-data:/var/lib/containerd"
    - "prepare-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
  registry:
    image: registry:2
    container_name: {{.RegistryHost}}
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
  prepare-containerd-data:
  prepare-containerd-stargz-grpc-data:
`, struct {
		TestImageName      string
		CRIServiceName     string
		PrepareServiceName string
		ImageContextDir    string
		RegistryHost       string
	}{
		TestImageName:      testImageName,
		CRIServiceName:     criServiceName,
		PrepareServiceName: prepareServiceName,
		ImageContextDir:    pRoot,
		RegistryHost:       registryHost,
	}),
		compose.WithBuildArgs(getBuildArgsFromEnv(t)...),
		compose.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to create CRI compose: %v", err)
	}
	de, ok := c.Get(criServiceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v", criServiceName)
	}
	deP, ok := c.Get(prepareServiceName)
	if !ok {
		t.Fatalf("failed to get shell of preparation service %v", prepareServiceName)
	}
	return shell.New(de, reporter), shell.New(deP, reporter), registryHostPort, c.Cleanup
}

func mirrorCRIImages(t *testing.T, sh *shell.Shell, registryHost string, imagesList []string) ([]string, map[string]string) {
	var (
		digests   = map[string]string{}
		digestsMu sync.Mutex
		hosts     = map[string]struct{}{}
		hostsMu   sync.Mutex
	)
	sh.Gox("containerd").Retry(100, "nerdctl", "version")

	ctx := context.Background()
	sem := semaphore.NewWeighted(int64(runtime.GOMAXPROCS(0)))
	eg := new(errgroup.Group)
	for _, image := range imagesList {
		image := image
		if err := sem.Acquire(ctx, 1); err != nil {
			t.Fatalf("failed to acquire semaphore: %v", err)
		}
		eg.Go(func() error {
			defer sem.Release(1)
			refspec, err := reference.Parse(image)
			if err != nil {
				return errors.Wrapf(err, "failed to parse %v", image)
			}
			hostsMu.Lock()
			hosts[refspec.Hostname()] = struct{}{}
			hostsMu.Unlock()
			u, err := url.Parse("dummy://" + refspec.Locator)
			if err != nil {
				return errors.Wrapf(err, "failed to parse path of image: %v", image)
			}
			mirrored := registryHost + "/" + strings.TrimPrefix(u.Path, "/")
			if tag, _ := reference.SplitObject(refspec.Object); tag != "" {
				mirrored = mirrored + ":" + tag
			}
			t.Logf("Mirroring: %v to %v\n", image, mirrored)
			sh.
				X("ctr-remote", "images", "pull", image).
				X("ctr-remote", "images", "optimize", "--oci", "--period=1", image, mirrored).
				X("ctr-remote", "images", "push", "--plain-http", mirrored)

			if orgDgst := refspec.Digest().String(); orgDgst != "" {
				err := sh.ForEach(shell.C("ctr-remote", "i", "ls", `name=="`+mirrored+`"`), func(l string) bool {
					if m := regexp.MustCompile(`(sha256:[a-z0-9]*)`).FindStringSubmatch(l); len(m) >= 2 {
						digestsMu.Lock()
						digests[orgDgst] = m[1]
						digestsMu.Unlock()
					}
					return true
				})
				if err != nil {
					return errors.Wrapf(err, "failed to run ctr-remote i ls")
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("failed to mirror images: %v", err)
	}
	var sources []string
	for h := range hosts {
		sources = append(sources, h)
	}
	return sources, digests
}

func appendFileContents(t *testing.T, sh *shell.Shell, path, addendum string) {
	if addendum == "" {
		return
	}
	contents := addendum
	if isFileExists(sh, path) {
		contents = strings.Join([]string{string(sh.O("cat", path)), contents}, "\n")
	}
	if err := testutil.WriteFileContents(sh, path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to append %v: %v", path, err)
	}
}
