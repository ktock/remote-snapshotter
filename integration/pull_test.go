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
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/rs/xid"
)

const (
	defaultContainerdConfigPath  = "/etc/containerd/config.toml"
	defaultSnapshotterConfigPath = "/etc/containerd-stargz-grpc/config.toml"
)

const proxySnapshotterConfig = `
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
`

// TestMultipleNamespaces tests to pull image from multiple namespaces
func TestMultipleNamespaces(t *testing.T) {
	t.Parallel()
	sh, done := newSnapshotterBaseShell(t)
	defer done()
	rebootContainerd(t, sh, "", "")
	image := "ghcr.io/stargz-containers/alpine:3.10.2-esgz"
	sh.X("ctr-remote", "--namespace=aaaa", "i", "rpull", image)
	sh.X("ctr-remote", "--namespace=bbbb", "i", "rpull", image)
}

// TestSnapshotterStartup tests to run containerd + snapshotter and check plugin is
// recognized by containerd
func TestSnapshotterStartup(t *testing.T) {
	t.Parallel()
	sh, done := newSnapshotterBaseShell(t)
	defer done()
	rebootContainerd(t, sh, "", "")
	found := false
	err := sh.ForEach(shell.C("ctr-remote", "plugin", "ls"), func(l string) bool {
		info := strings.Fields(l)
		if len(info) < 4 {
			t.Fatalf("mulformed plugin info: %v", info)
		}
		if info[0] == "io.containerd.snapshotter.v1" && info[1] == "stargz" && info[3] == "ok" {
			found = true
			return false
		}
		return true
	})
	if err != nil || !found {
		t.Fatalf("failed to get stargz snapshotter status using ctr-remote plugin ls: %v", err)
	}
}

// TestMirror tests if mirror & refreshing functionalities of snapshotter work
func TestMirror(t *testing.T) {
	t.Parallel()
	var (
		reporter        = testutil.NewTestingReporter(t)
		pRoot           = testutil.GetProjectRoot(t)
		caCertDir       = "/usr/local/share/ca-certificates"
		registryHost    = "registry-" + xid.New().String() + ".test"
		registryAltHost = "registry-alt-" + xid.New().String() + ".test"
		registryUser    = "dummyuser"
		registryPass    = "dummypass"
		registryCreds   = func() string { return registryUser + ":" + registryPass }
		serviceName     = "testing_mirror"
	)
	ghcr := func(name string) imageInfo {
		return imageInfo{"ghcr.io/stargz-containers/" + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}
	mirror2 := func(name string) imageInfo {
		return imageInfo{registryAltHost + ":5000/" + name, "", true}
	}

	// Setup dummy creds for test
	crt, key, err := generateRegistrySelfSignedCert(registryHost)
	if err != nil {
		t.Fatalf("failed to generate cert: %v", err)
	}
	htpasswd, err := generateBasicHtpasswd(registryUser, registryPass)
	if err != nil {
		t.Fatalf("failed to generate htpasswd: %v", err)
	}
	authDir, err := ioutil.TempDir("", "tmpcontext")
	if err != nil {
		t.Fatalf("failed to prepare auth tmpdir")
	}
	defer os.RemoveAll(authDir)
	if err := ioutil.WriteFile(filepath.Join(authDir, "domain.key"), key, 0666); err != nil {
		t.Fatalf("failed to prepare key file")
	}
	if err := ioutil.WriteFile(filepath.Join(authDir, "domain.crt"), crt, 0666); err != nil {
		t.Fatalf("failed to prepare crt file")
	}
	if err := ioutil.WriteFile(filepath.Join(authDir, "htpasswd"), htpasswd, 0666); err != nil {
		t.Fatalf("failed to prepare htpasswd file")
	}

	targetStage := "snapshotter-base"
	if isTestingBuiltinSnapshotter() {
		targetStage = "containerd-snapshotter-base"
	}

	// Run testing environment on docker compose
	c, err := compose.New(testutil.ApplyTextTemplate(t, `
version: "3.7"
services:
  {{.ServiceName}}:
    build:
      context: {{.ImageContextDir}}
      target: {{.TargetStage}}
      args:
      - SNAPSHOTTER_BUILD_FLAGS="-race"
    privileged: true
    init: true
    entrypoint: [ "sleep", "infinity" ]
    environment:
    - NO_PROXY=127.0.0.1,localhost,{{.RegistryHost}}:443
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - /dev/fuse:/dev/fuse
    - "lazy-containerd-data:/var/lib/containerd"
    - "lazy-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
  registry:
    image: registry:2
    container_name: {{.RegistryHost}}
    environment:
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/domain.key
    - REGISTRY_HTTP_ADDR={{.RegistryHost}}:443
    volumes:
    - {{.AuthDir}}:/auth:ro
  registry-alt:
    image: registry:2
    container_name: {{.RegistryAltHost}}
volumes:
  lazy-containerd-data:
  lazy-containerd-stargz-grpc-data:
`, struct {
		TargetStage     string
		ServiceName     string
		ImageContextDir string
		RegistryHost    string
		RegistryAltHost string
		AuthDir         string
	}{
		TargetStage:     targetStage,
		ServiceName:     serviceName,
		ImageContextDir: pRoot,
		RegistryHost:    registryHost,
		RegistryAltHost: registryAltHost,
		AuthDir:         authDir,
	}),
		compose.WithBuildArgs(getBuildArgsFromEnv(t)...),
		compose.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to prepare compose: %v", err)
	}
	defer c.Cleanup()
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v: %v", serviceName, err)
	}
	sh := shell.New(de, reporter)

	// Initialize config files for containerd and snapshotter
	additionalConfig := ""
	if !isTestingBuiltinSnapshotter() {
		additionalConfig = proxySnapshotterConfig
	}
	containerdConfigYaml := testutil.ApplyTextTemplate(t, `
version = 2

[plugins."io.containerd.snapshotter.v1.stargz"]
root_path = "/var/lib/containerd-stargz-grpc/"

[plugins."io.containerd.snapshotter.v1.stargz".blob]
check_always = true

[[plugins."io.containerd.snapshotter.v1.stargz".resolver.host."{{.RegistryHost}}".mirrors]]
host = "{{.RegistryAltHost}}:5000"
insecure = true

{{.AdditionalConfig}}
`, struct {
		RegistryHost     string
		RegistryAltHost  string
		AdditionalConfig string
	}{
		RegistryHost:     registryHost,
		RegistryAltHost:  registryAltHost,
		AdditionalConfig: additionalConfig,
	})
	snapshotterConfigYaml := testutil.ApplyTextTemplate(t, `
[blob]
check_always = true

[[resolver.host."{{.RegistryHost}}".mirrors]]
host = "{{.RegistryAltHost}}:5000"
insecure = true
`, struct {
		RegistryHost    string
		RegistryAltHost string
	}{
		RegistryHost:    registryHost,
		RegistryAltHost: registryAltHost,
	})

	// Setup environment
	if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, []byte(containerdConfigYaml), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultContainerdConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, defaultSnapshotterConfigPath, []byte(snapshotterConfigYaml), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultSnapshotterConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, filepath.Join(caCertDir, "domain.crt"), crt, 0600); err != nil {
		t.Fatalf("failed to write %v: %v", caCertDir, err)
	}
	sh.
		X("apt-get", "--no-install-recommends", "install", "-y", "iptables").
		X("update-ca-certificates").
		Retry(100, "nerdctl", "login", "-u", registryUser, "-p", registryPass, registryHost)

	// Mirror images
	rebootContainerd(t, sh, "", "")
	copyImage(sh, ghcr("alpine:3.13-org"), mirror("alpine:3.13"))
	optimizeImage(sh, mirror("alpine:3.13"), mirror("alpine:esgz"))
	optimizeImage(sh, mirror("alpine:3.13"), mirror2("alpine:esgz"))

	// Pull images
	// NOTE: Registry connection will still be checked on each "run" because
	//       we added "check_always = true" to the configuration in the above.
	//       We use this behaviour for testing mirroring & refleshing functionality.
	rebootContainerd(t, sh, "", "")
	sh.X("ctr-remote", "i", "pull", "--user", registryCreds(), mirror("alpine:esgz").ref)
	sh.X("ctr-remote", "i", "rpull", "--user", registryCreds(), mirror("alpine:esgz").ref)
	registryHostIP, registryAltHostIP := getIP(t, sh, registryHost), getIP(t, sh, registryAltHost)
	export := func(image string) []string {
		return shell.C("ctr-remote", "run", "--rm", "--snapshotter=stargz", image, "test", "tar", "-c", "/usr")
	}
	sample := func(tarExportArgs ...string) {
		sh.Pipe(nil, shell.C("ctr-remote", "run", "--rm", mirror("alpine:esgz").ref, "test", "tar", "-c", "/usr"), tarExportArgs)
	}

	// test if mirroring is working (switching to registryAltHost)
	testSameTarContents(t, sh, sample,
		func(tarExportArgs ...string) {
			sh.
				X("iptables", "-A", "OUTPUT", "-d", registryHostIP, "-j", "DROP").
				X("iptables", "-L").
				Pipe(nil, export(mirror("alpine:esgz").ref), tarExportArgs).
				X("iptables", "-D", "OUTPUT", "-d", registryHostIP, "-j", "DROP")
		},
	)

	// test if refreshing is working (swithching back to registryHost)
	testSameTarContents(t, sh, sample,
		func(tarExportArgs ...string) {
			sh.
				X("iptables", "-A", "OUTPUT", "-d", registryAltHostIP, "-j", "DROP").
				X("iptables", "-L").
				Pipe(nil, export(mirror("alpine:esgz").ref), tarExportArgs).
				X("iptables", "-D", "OUTPUT", "-d", registryAltHostIP, "-j", "DROP")
		},
	)
}

// TestLazyPull tests if lazy pulling works.
func TestLazyPull(t *testing.T) {
	t.Parallel()
	var (
		registryHost  = "registry-" + xid.New().String() + ".test"
		registryUser  = "dummyuser"
		registryPass  = "dummypass"
		registryCreds = func() string { return registryUser + ":" + registryPass }
	)
	ghcr := func(name string) imageInfo {
		return imageInfo{"ghcr.io/stargz-containers/" + name, "", false}
	}
	mirror := func(name string) imageInfo {
		return imageInfo{registryHost + "/" + name, registryUser + ":" + registryPass, false}
	}

	// Prepare config for containerd and snapshotter
	getContainerdConfigYaml := func(disableVerification bool) []byte {
		additionalConfig := ""
		if !isTestingBuiltinSnapshotter() {
			additionalConfig = proxySnapshotterConfig
		}
		return []byte(testutil.ApplyTextTemplate(t, `
version = 2

[plugins."io.containerd.snapshotter.v1.stargz"]
root_path = "/var/lib/containerd-stargz-grpc/"
disable_verification = {{.DisableVerification}}

[plugins."io.containerd.snapshotter.v1.stargz".blob]
check_always = true

[debug]
format = "json"
level = "debug"

{{.AdditionalConfig}}
`, struct {
			DisableVerification bool
			AdditionalConfig    string
		}{
			DisableVerification: disableVerification,
			AdditionalConfig:    additionalConfig,
		}))
	}
	getSnapshotterConfigYaml := func(disableVerification bool) []byte {
		return []byte(fmt.Sprintf("disable_verification = %v", disableVerification))
	}

	// Setup environment
	sh, _, done := newShellWithRegistry(t, registryHost, registryUser, registryPass)
	defer done()
	if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, getContainerdConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultContainerdConfigPath, err)
	}
	if err := testutil.WriteFileContents(sh, defaultSnapshotterConfigPath, getSnapshotterConfigYaml(false), 0600); err != nil {
		t.Fatalf("failed to write %v: %v", defaultSnapshotterConfigPath, err)
	}
	sh.X("go", "get", "github.com/google/crfs/stargz/stargzify")

	// Mirror images
	rebootContainerd(t, sh, "", "")
	copyImage(sh, ghcr("ubuntu:20.04-org"), mirror("ubuntu:20.04"))
	sh.X("stargzify", mirror("ubuntu:20.04").ref, mirror("ubuntu:sgz").ref)
	optimizeImage(sh, mirror("ubuntu:20.04"), mirror("ubuntu:esgz"))

	// Test if contents are pulled
	fromNormalSnapshotter := func(image string) tarPipeExporter {
		return func(tarExportArgs ...string) {
			rebootContainerd(t, sh, "", "")
			sh.
				X("ctr-remote", "images", "pull", "--user", registryCreds(), image).
				Pipe(nil, shell.C("ctr-remote", "run", "--rm", image, "test", "tar", "-c", "/usr"), tarExportArgs)
		}
	}
	export := func(sh *shell.Shell, image string, tarExportArgs []string) {
		sh.X("ctr-remote", "images", "rpull", "--user", registryCreds(), image)
		sh.Pipe(nil, shell.C("ctr-remote", "run", "--rm", "--snapshotter=stargz", image, "test", "tar", "-c", "/usr"), tarExportArgs)
	}
	// NOTE: these tests must be executed sequentially.
	tests := []struct {
		name string
		want tarPipeExporter
		test tarPipeExporter
	}{
		{
			name: "normal",
			want: fromNormalSnapshotter(mirror("ubuntu:20.04").ref),
			test: func(tarExportArgs ...string) {
				image := mirror("ubuntu:20.04").ref
				rebootContainerd(t, sh, "", "")
				export(sh, image, tarExportArgs)
			},
		},
		{
			name: "eStargz",
			want: fromNormalSnapshotter(mirror("ubuntu:20.04").ref),
			test: func(tarExportArgs ...string) {
				image := mirror("ubuntu:esgz").ref
				m := rebootContainerd(t, sh, "", "")
				export(sh, image, tarExportArgs)
				m.CheckAllRemoteSnapshots(t)
			},
		},
		{
			name: "legacy stargz",
			want: fromNormalSnapshotter(mirror("ubuntu:sgz").ref),
			test: func(tarExportArgs ...string) {
				image := mirror("ubuntu:sgz").ref
				var m *testutil.RemoteSnapshotMonitor
				if isTestingBuiltinSnapshotter() {
					m = rebootContainerd(t, sh, string(getContainerdConfigYaml(true)), "")
				} else {
					m = rebootContainerd(t, sh, "", string(getSnapshotterConfigYaml(true)))
				}
				export(sh, image, tarExportArgs)
				m.CheckAllRemoteSnapshots(t)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testSameTarContents(t, sh, tt.want, tt.test)
		})
	}
}

func getIP(t *testing.T, sh *shell.Shell, name string) string {
	resolved := strings.Fields(string(sh.O("getent", "hosts", name)))
	if len(resolved) < 1 {
		t.Fatalf("failed to resolve name %v", name)
	}
	return resolved[0]
}

type tarPipeExporter func(tarExportArgs ...string)

func testSameTarContents(t *testing.T, sh *shell.Shell, aC, bC tarPipeExporter) {
	aDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir A: %v", err)
	}
	bDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir B: %v", err)
	}
	aC("tar", "-xC", aDir)
	bC("tar", "-xC", bDir)
	sh.X("diff", "--no-dereference", "-qr", aDir+"/", bDir+"/")
}

type imageInfo struct {
	ref       string
	creds     string
	plainHTTP bool
}

func encodeImageInfo(ii ...imageInfo) [][]string {
	var opts [][]string
	for _, i := range ii {
		var o []string
		if i.creds != "" {
			o = append(o, "-u", i.creds)
		}
		if i.plainHTTP {
			o = append(o, "--plain-http")
		}
		o = append(o, i.ref)
		opts = append(opts, o)
	}
	return opts
}

func copyImage(sh *shell.Shell, src, dst imageInfo) {
	opts := encodeImageInfo(src, dst)
	sh.
		X(append([]string{"ctr-remote", "i", "pull", "--all-platforms"}, opts[0]...)...).
		X("ctr-remote", "i", "tag", src.ref, dst.ref).
		X(append([]string{"ctr-remote", "i", "push"}, opts[1]...)...)
}

func optimizeImage(sh *shell.Shell, src, dst imageInfo) {
	opts := encodeImageInfo(src, dst)
	sh.
		X(append([]string{"ctr-remote", "i", "pull", "--all-platforms"}, opts[0]...)...).
		X("ctr-remote", "i", "optimize", "--oci", src.ref, dst.ref).
		X(append([]string{"ctr-remote", "i", "push"}, opts[1]...)...)
}

func rebootContainerd(t *testing.T, sh *shell.Shell, customContainerdConfig, customSnapshotterConfig string) *testutil.RemoteSnapshotMonitor {
	var (
		containerdRoot    = "/var/lib/containerd/"
		containerdStatus  = "/run/containerd/"
		snapshotterSocket = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
		snapshotterRoot   = "/var/lib/containerd-stargz-grpc/"
	)

	// cleanup directories
	testutil.KillMatchingProcess(sh, "containerd")
	testutil.KillMatchingProcess(sh, "containerd-stargz-grpc")
	removeUnder(sh, containerdRoot)
	if isDirExists(sh, containerdStatus) {
		removeUnder(sh, containerdStatus)
	}
	if isFileExists(sh, snapshotterSocket) {
		sh.X("rm", snapshotterSocket)
	}
	if snDir := filepath.Join(snapshotterRoot, "/snapshotter/snapshots"); isDirExists(sh, snDir) {
		sh.X("find", snDir, "-maxdepth", "1", "-mindepth", "1", "-type", "d",
			"-exec", "umount", "{}/fs", ";")
	}
	removeUnder(sh, snapshotterRoot)

	// run containerd and snapshotter
	var m *testutil.RemoteSnapshotMonitor
	if isTestingBuiltinSnapshotter() {
		containerdCmds := shell.C("containerd", "--log-level", "debug")
		if customContainerdConfig != "" {
			containerdCmds = addConfig(t, sh, customContainerdConfig, containerdCmds...)
		}
		outR, errR, err := sh.R(containerdCmds...)
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		m = testutil.NewRemoteSnapshotMonitor(testutil.NewTestingReporter(t), outR, errR)
	} else {
		snapshotterCmds := shell.C("containerd-stargz-grpc", "--log-level", "debug",
			"--address", snapshotterSocket)
		if customSnapshotterConfig != "" {
			snapshotterCmds = addConfig(t, sh, customSnapshotterConfig, snapshotterCmds...)
		}
		outR, errR, err := sh.R(snapshotterCmds...)
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		m = testutil.NewRemoteSnapshotMonitor(testutil.NewTestingReporter(t), outR, errR)
		containerdCmds := shell.C("containerd", "--log-level", "debug")
		if customContainerdConfig != "" {
			containerdCmds = addConfig(t, sh, customContainerdConfig, containerdCmds...)
		}
		sh.Gox(containerdCmds...)
	}

	// make sure containerd and containerd-stargz-grpc are up-and-running
	sh.Retry(100, "ctr", "snapshots", "--snapshotter", "stargz",
		"prepare", "connectiontest-dummy-"+xid.New().String(), "")

	return m
}

func removeUnder(sh *shell.Shell, dir string) {
	sh.X("find", dir+"/.", "!", "-name", ".", "-prune", "-exec", "rm", "-rf", "{}", "+")
}

func addConfig(t *testing.T, sh *shell.Shell, conf string, cmds ...string) []string {
	configPath := strings.TrimSpace(string(sh.O("mktemp")))
	if err := testutil.WriteFileContents(sh, configPath, []byte(conf), 0600); err != nil {
		t.Fatalf("failed to add config to %v: %v", configPath, err)
	}
	return append(cmds, "--config", configPath)
}
