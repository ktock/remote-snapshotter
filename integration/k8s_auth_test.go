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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/snapshot"
	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/containerd/stargz-snapshotter/util/dockershell/kind"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/rs/xid"
)

const snKubeConfigPath = "/etc/kubernetes/snapshotter/config.conf"

// TestPullSecretsOnK8s tests secret-based authorization works on Kubernetes.
func TestPullSecretsOnK8s(t *testing.T) {
	t.Parallel()
	var (
		registryHost     = "registry" + xid.New().String() + ".test"
		registryUser     = "dummyuser"
		registryPass     = "dummypass"
		registryCreds    = func() string { return registryUser + ":" + registryPass }
		networkName      = "testnetwork-" + xid.New().String()
		orgImage         = "ghcr.io/stargz-containers/ubuntu:20.04"
		mirrorImage      = registryHost + "/library/ubuntu:20.04"
		registryCertPath = "/usr/local/share/ca-certificates/registry.crt"
		snServiceAccount = "stargz-snapshotter"
		testNamespace    = "ns1"
	)

	// Setup registry and mirror the testing image
	shP, crtData, done := newShellWithRegistry(t,
		registryHost, registryUser, registryPass, withNetwork(networkName))
	defer done()
	shP.
		Gox("containerd").
		Retry(100, "nerdctl", "version").
		X("ctr-remote", "i", "pull", orgImage).
		X("ctr-remote", "i", "optimize", "--oci", "--period=1", orgImage, mirrorImage).
		X("ctr-remote", "i", "push", "-u", registryCreds(), mirrorImage)

	// Create KinD cluster
	workingNode, k, sh := createTestingCluster(t, registryHost, registryCertPath, crtData, networkName)
	defer k.Cleanup()

	// Create Service Account for snapshotter
	err := kApply(k, testutil.ApplyTextTemplate(t, `
apiVersion: v1
kind: Namespace
metadata:
  name: {{.TestNamespace}}
---
apiVersion: v1
kind: Secret
metadata:
  name: testsecret
  namespace: {{.TestNamespace}}
data:
  .dockerconfigjson: {{.DockerConfigBase64}}
type: kubernetes.io/dockerconfigjson
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.SNServiceAccount}}
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: stargz-snapshotter
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: stargz-snapshotter
subjects:
- kind: ServiceAccount
  name: stargz-snapshotter
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: stargz-snapshotter
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list", "watch"]
`, struct {
		SNServiceAccount   string
		TestNamespace      string
		DockerConfigBase64 string
	}{
		SNServiceAccount: snServiceAccount,
		TestNamespace:    testNamespace,
		DockerConfigBase64: base64.StdEncoding.EncodeToString(
			[]byte(createDockerConfigJSON(t, registryHost, registryUser, registryPass))),
	}))
	if err != nil {
		t.Fatalf("failed to create SA and secrets: %v", err)
	}

	// Install snapshotter's ServiceAccount kubeconfig to the node and configure the node
	err = testutil.WriteFileContents(sh, snKubeConfigPath,
		createServiceAccountKubeconfig(t, k, snServiceAccount,
			"https://"+workingNode+":"+apiServerPort(t, sh)), 0600)
	if err != nil {
		t.Fatalf("failed to write snapshotter kubeconfig %v: %v", snKubeConfigPath, err)
	}
	time.Sleep(30 * time.Second) // wait until the secrets are fully synced to snapshotter

	// Create sample pod
	testPodName := "testpod-" + xid.New().String()
	testContainerName := "testcontainer-" + xid.New().String()
	err = kApply(k, testutil.ApplyTextTemplate(t, `
apiVersion: v1
kind: Pod
metadata:
  name: {{.TestPodName}}
  namespace: {{.TestPodNamespace}}
spec:
  containers:
  - name: {{.TestContainerName}}
    image: {{.TestImageName}}
    command: ["sleep"]
    args: ["infinity"]
  imagePullSecrets:
  - name: testsecret
`, struct {
		TestPodName       string
		TestPodNamespace  string
		TestContainerName string
		TestImageName     string
	}{
		TestPodName:       testPodName,
		TestPodNamespace:  testNamespace,
		TestContainerName: testContainerName,
		TestImageName:     mirrorImage,
	}))
	if err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	waitUntilPodCreated(t, k, testPodName, testNamespace)

	// Check if the container is created with remote snapshots
	checkContainerRemote(t, sh, testContainerName)
}

func createTestingCluster(t *testing.T, registryHost, registryCertPath string, crtData []byte, networkName string) (string, *kind.Kind, *shell.Shell) {
	pRoot := testutil.GetProjectRoot(t)
	targetStage := ""
	if isTestingBuiltinSnapshotter() {
		targetStage = "kind-builtin-snapshotter"
	}
	snapshotterAddConfig := fmt.Sprintf(`
[kubeconfig_keychain]
enable_keychain = true
kubeconfig_path = "%s"
`, snKubeConfigPath)
	containerdAddConfig := fmt.Sprintf(`
[plugins."io.containerd.grpc.v1.cri".registry.configs."%s".tls]
ca_file = "%s"
`, registryHost, registryCertPath)
	if isTestingBuiltinSnapshotter() {
		containerdAddConfig += fmt.Sprintf(`
[plugins."io.containerd.snapshotter.v1.stargz".kubeconfig_keychain]
enable_keychain = true
kubeconfig_path = "%s"
`, snKubeConfigPath)
	}
	tmpContext, err := ioutil.TempDir("", "tmpcontext")
	if err != nil {
		t.Fatalf("failed to prepare tmp context")
	}
	defer os.RemoveAll(tmpContext)
	if err := ioutil.WriteFile(filepath.Join(tmpContext, "snadd.toml"), []byte(snapshotterAddConfig), 0666); err != nil {
		t.Fatalf("failed to prepare snapshotter config")
	}
	if err := ioutil.WriteFile(filepath.Join(tmpContext, "cdadd.toml"), []byte(containerdAddConfig), 0666); err != nil {
		t.Fatalf("failed to prepare containerd config")
	}
	if err := ioutil.WriteFile(filepath.Join(tmpContext, "registry.crt"), crtData, 0666); err != nil {
		t.Fatalf("failed to prepare registry cert")
	}
	nodeImage, iDone, err := dexec.NewTempImage(pRoot, targetStage,
		dexec.WithTempImageBuildArgs(getBuildArgsFromEnv(t)...),
		dexec.WithTempImageStdio(testutil.TestingLogDest()),
		dexec.WithPatchContextDir(tmpContext),
		dexec.WithPatchDockerfile(fmt.Sprintf(`
COPY . /tmp/
RUN mkdir -p /etc/containerd-stargz-grpc /etc/containerd %s && \
    cat /tmp/snadd.toml >> /etc/containerd-stargz-grpc/config.toml && \
    cat /tmp/cdadd.toml >> /etc/containerd/config.toml && \
    cat /tmp/registry.crt > %s && \
    update-ca-certificates
`, filepath.Dir(registryCertPath), registryCertPath)))
	if err != nil {
		t.Fatalf("failed to build image: %v", err)
	}
	defer iDone()
	k, err := kind.New(testutil.ApplyTextTemplate(t, `
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: {{.KindNodeImage}}
`, struct {
		KindNodeImage string
	}{
		KindNodeImage: nodeImage,
	}), kind.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to create KinD cluster: %v", err)
	}
	shNames := k.List()
	for _, name := range shNames {
		// Connect all nodes to the network where a registry is running.
		de, ok := k.Get(name)
		if !ok {
			t.Fatalf("kind node %v not found", name)
		}
		if err := dexec.Connect(de, networkName); err != nil {
			t.Fatalf("failed to connect kind node %v to NW %v: %v", name, networkName, err)
		}
	}
	if len(shNames) <= 0 {
		t.Fatalf("kind cluster didn't cretaed")
	}
	workingNode := shNames[0]
	de, ok := k.Get(workingNode)
	if !ok {
		t.Fatalf("node %v not found", workingNode)
	}
	sh := shell.New(de, testutil.NewTestingReporter(t))
	return workingNode, k, sh
}

func apiServerPort(t *testing.T, controlPlane *shell.Shell) (apiserverPort string) {
	err := controlPlane.ForEach(shell.C("ps", "auxww"), func(l string) bool {
		if m := regexp.MustCompile(`--secure-port=([0-9]*)`).FindStringSubmatch(l); len(m) >= 2 {
			apiserverPort = m[1]
			return false
		}
		return true
	})
	if err != nil || apiserverPort == "" {
		t.Fatalf("failed to get apiserver port using ps: %v", err)
	}
	return apiserverPort
}

func waitUntilPodCreated(t *testing.T, k *kind.Kind, podname, namespace string) {
	var started bool
	for i := 0; i < 100; i++ {
		status, err := k.KubeCtl("get", "pods", podname, "--namespace", namespace,
			"-o", `jsonpath={..status.containerStatuses[0].state.running.startedAt}${..status.containerStatuses[0].state.waiting.reason}`).Output()
		if err != nil {
			t.Fatalf("failed to get pods: %v", err)
		}
		t.Logf("Status: %v\n", string(status))
		s := strings.Split(string(status), "$")
		if len(s) < 2 {
			t.Fatalf("mulformed status of pod %v: %v", podname, status)
		}
		if startedAt := s[0]; startedAt != "" {
			started = true
			break
		}
		time.Sleep(time.Second)
	}
	if !started {
		t.Fatalf("pod hasn't started")
	}
}

func createServiceAccountKubeconfig(t *testing.T, k *kind.Kind, serviceAccount, apiServerAddr string) []byte {
	var tokenname string
	for i := 0; i < 50; i++ {
		tokennameData, err := k.KubeCtl("get", "sa", serviceAccount,
			"-o", `jsonpath={.secrets[0].name}`).Output()
		if err == nil {
			tokenname = string(tokennameData)
			break
		}
		testutil.TestingL.Printf("failed to get SA of %v: %v", serviceAccount, err)
		dump, err := k.KubeCtl("get", "sa", "-A").Output()
		testutil.TestingL.Printf("dump Service Acounts: %v: %v", string(dump), err)
		time.Sleep(3 * time.Second)
	}
	if tokenname == "" {
		t.Fatalf("failed to get token name of stargz snapshotter service account")
	}
	ca, err := k.KubeCtl("get", "secret/"+tokenname, "-o", `jsonpath={.data.ca\.crt}`).Output()
	if err != nil {
		t.Fatalf("failed to get secret of snapshotter sa : %v", err)
	}
	tokenB, err := k.KubeCtl("get", "secret/"+tokenname, "-o", `jsonpath={.data.token}`).Output()
	if err != nil {
		t.Fatalf("failed to get token of snapshotter sa : %v", err)
	}
	token, err := base64.StdEncoding.DecodeString(string(tokenB))
	if err != nil {
		t.Fatalf("failed to decode token: %v", err)
	}
	return []byte(testutil.ApplyTextTemplate(t, `
apiVersion: v1
kind: Config
clusters:
- name: default-cluster
  cluster:
    certificate-authority-data: {{.CA}}
    server: {{.APIServerAddr}}
contexts:
- name: default-context
  context:
    cluster: default-cluster
    namespace: default
    user: default-user
current-context: default-context
users:
- name: default-user
  user:
    token: {{.Token}}
`, struct {
		CA            string
		APIServerAddr string
		Token         string
	}{
		CA:            string(ca),
		APIServerAddr: apiServerAddr,
		Token:         string(token),
	}))
}

func checkContainerRemote(t *testing.T, sh *shell.Shell, testContainerName string) {
	var gotContainer string
	for i := 0; i < 100; i++ {
		out := sh.O("ctr-remote", "--namespace=k8s.io", "c", "ls",
			"-q", `labels.io.kubernetes.container.name==`+testContainerName+``)
		if c := strings.TrimSpace(string(out)); c != "" {
			gotContainer = c
			break
		}
		time.Sleep(time.Second)
	}
	if gotContainer == "" {
		sh.X("ctr-remote", "--namespace=k8s.io", "c", "ls")
		t.Fatalf("container hasn't been created")
	}

	snKey := struct{ SnapshotKey string }{}
	if err := json.Unmarshal(
		sh.O("ctr-remote", "--namespace=k8s.io", "c", "info", gotContainer),
		&snKey,
	); err != nil {
		t.Fatalf("failed to parse ctr output of %v: %v", gotContainer, err)
	}
	snapshotKey := snKey.SnapshotKey
	var complete bool
	// NOTE: We don't check the topmost *active* (non-lazy) snapshot
	for i := 0; i < 100; i++ {
		parent := struct{ Parent string }{}
		if err := json.Unmarshal(
			sh.O("ctr-remote", "--namespace=k8s.io", "snapshot",
				"--snapshotter=stargz", "info", snapshotKey),
			&parent,
		); err != nil {
			t.Fatalf("failed to parse ctr output of %v: %v", gotContainer, err)
		}
		snapshotKey = parent.Parent
		if snapshotKey == "" {
			complete = true // reached the bottommost layer
			break
		}
		label := struct{ Labels map[string]string }{}
		out := sh.O("ctr-remote", "--namespace=k8s.io", "snapshot",
			"--snapshotter=stargz", "info", snapshotKey)
		if err := json.Unmarshal(out, &label); err != nil || label.Labels == nil {
			t.Fatalf("failed to parse label of snapshot %v = %v: %v",
				snapshotKey, string(out), err)
		}
		if v, ok := label.Labels[snapshot.RemoteLabel]; !ok || v != snapshot.RemoteLabelVal {
			t.Fatalf("snapshot %v is not remote snapshot", snapshotKey)
		}
	}
	if !complete {
		t.Fatalf("testing image contains too many layes > 100")
	}
}

func createDockerConfigJSON(t *testing.T, registryHost, user, pass string) string {
	return testutil.ApplyTextTemplate(t, `{"auths":{"{{.RegistryHost}}":{"auth":"{{.CredsBase64}}"}}}`, struct {
		RegistryHost string
		CredsBase64  string
	}{
		RegistryHost: registryHost,
		CredsBase64:  base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
	})
}

func kApply(k *kind.Kind, configYaml string) error {
	cmd := k.KubeCtl("apply", "-f", "-")
	cmd.Stdin = bytes.NewReader([]byte(configYaml))
	return cmd.Run()
}
