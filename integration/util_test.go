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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/csv"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"golang.org/x/crypto/bcrypt"
)

const (
	builtinSnapshotterFlagEnv = "BUILTIN_SNAPSHOTTER"
	buildArgsEnv              = "DOCKER_BUILD_ARGS"
)

func isTestingBuiltinSnapshotter() bool {
	return os.Getenv(builtinSnapshotterFlagEnv) == "true"
}

func getBuildArgsFromEnv(t *testing.T) []string {
	buildArgsStr := os.Getenv(buildArgsEnv)
	if buildArgsStr == "" {
		return nil
	}
	r := csv.NewReader(strings.NewReader(buildArgsStr))
	buildArgs, err := r.Read()
	if err != nil {
		t.Fatalf("failed to get build args from env %v", buildArgsEnv)
	}
	return buildArgs
}

func isFileExists(sh *shell.Shell, file string) bool {
	return sh.Command("test", "-f", file).Run() == nil
}

func isDirExists(sh *shell.Shell, dir string) bool {
	return sh.Command("test", "-d", dir).Run() == nil
}

type registryOptions struct {
	network string
}

type registryOpt func(o *registryOptions)

func withNetwork(nw string) registryOpt {
	return func(o *registryOptions) {
		o.network = nw
	}
}

func newShellWithRegistry(t *testing.T, registryHost, registryUser, registryPass string, opts ...registryOpt) (sh *shell.Shell, crtData []byte, done func() error) {
	var rOpts registryOptions
	for _, o := range opts {
		o(&rOpts)
	}
	var (
		pRoot       = testutil.GetProjectRoot(t)
		caCertDir   = "/usr/local/share/ca-certificates"
		serviceName = "testing"
	)

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
	cOpts := []compose.Option{
		compose.WithBuildArgs(getBuildArgsFromEnv(t)...),
		compose.WithStdio(testutil.TestingLogDest()),
	}
	networkConfig := ""
	var cleanups []func() error
	if nw := rOpts.network; nw != "" {
		done, err := dexec.NewTempNetwork(nw)
		if err != nil {
			t.Fatalf("failed to create temp network %v: %v", nw, err)
		}
		cleanups = append(cleanups, done)
		networkConfig = fmt.Sprintf(`
networks:
  default:
    external:
      name: %s
`, nw)
	}
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
volumes:
  lazy-containerd-data:
  lazy-containerd-stargz-grpc-data:
{{.NetworkConfig}}
`, struct {
		ServiceName     string
		ImageContextDir string
		TargetStage     string
		RegistryHost    string
		AuthDir         string
		NetworkConfig   string
	}{
		ServiceName:     serviceName,
		ImageContextDir: pRoot,
		TargetStage:     targetStage,
		RegistryHost:    registryHost,
		AuthDir:         authDir,
		NetworkConfig:   networkConfig,
	}), cOpts...)
	if err != nil {
		t.Fatalf("failed to prepare compose: %v", err)
	}
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v", serviceName)
	}
	sh = shell.New(de, testutil.NewTestingReporter(t))

	// Install cert and login to the registry
	if err := testutil.WriteFileContents(sh, filepath.Join(caCertDir, "domain.crt"), crt, 0600); err != nil {
		t.Fatalf("failed to write cert at %v: %v", caCertDir, err)
	}
	sh.
		X("update-ca-certificates").
		Retry(100, "nerdctl", "login", "-u", registryUser, "-p", registryPass, registryHost)
	return sh, crt, func() error {
		if err := c.Cleanup(); err != nil {
			return err
		}
		for _, f := range cleanups {
			if err := f(); err != nil {
				return err
			}
		}
		return os.RemoveAll(authDir)
	}
}

func newSnapshotterBaseShell(t *testing.T) (*shell.Shell, func() error) {
	var (
		pRoot       = testutil.GetProjectRoot(t)
		serviceName = "testing"
	)
	targetStage := "snapshotter-base"
	if isTestingBuiltinSnapshotter() {
		targetStage = "containerd-snapshotter-base"
	}
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
    - NO_PROXY=127.0.0.1,localhost
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - /dev/fuse:/dev/fuse
    - "containerd-data:/var/lib/containerd"
    - "containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
`, struct {
		ServiceName     string
		ImageContextDir string
		TargetStage     string
	}{
		ServiceName:     serviceName,
		ImageContextDir: pRoot,
		TargetStage:     targetStage,
	}),
		compose.WithBuildArgs(getBuildArgsFromEnv(t)...),
		compose.WithStdio(testutil.TestingLogDest()),
	)
	if err != nil {
		t.Fatalf("failed to prepare compose: %v", err)
	}
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v", serviceName)
	}
	sh := shell.New(de, testutil.NewTestingReporter(t))
	if !isTestingBuiltinSnapshotter() {
		if err := testutil.WriteFileContents(sh, defaultContainerdConfigPath, []byte(proxySnapshotterConfig), 0600); err != nil {
			t.Fatalf("failed to wrtie containerd config %v: %v", defaultContainerdConfigPath, err)
		}
	}
	return sh, c.Cleanup
}

func generateRegistrySelfSignedCert(registryHost string) (crt, key []byte, _ error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 60)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		IsCA:                  true,
		BasicConstraintsValid: true,
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: registryHost},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{registryHost},
	}
	privatekey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	publickey := &privatekey.PublicKey
	cert, err := x509.CreateCertificate(rand.Reader, &template, &template, publickey, privatekey)
	if err != nil {
		return nil, nil, err
	}
	certPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})
	privBytes, err := x509.MarshalPKCS8PrivateKey(privatekey)
	if err != nil {
		return nil, nil, err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	return certPem, keyPem, nil
}

func generateBasicHtpasswd(user, pass string) ([]byte, error) {
	bpass, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return []byte(user + ":" + string(bpass) + "\n"), nil
}
