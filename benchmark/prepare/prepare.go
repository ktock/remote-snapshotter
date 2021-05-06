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

package prepare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/stargz-snapshotter/benchmark/types"
	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/rs/xid"
)

const (
	prepareDockerConfigEnv = "PREPARE_DOCKER_CONFIG"
	srcRepo                = "docker.io/library"
)

func Supported() error {
	if err := shell.Supported(); err != nil {
		return err
	}
	return compose.Supported()
}

// Preparer prepares images using ctr-remote and containerd's converter API
type Preparer struct {
	c  *compose.Compose
	sh *shell.Shell
}

func NewPreparer(t *testing.T) *Preparer {
	var (
		pRoot           = testutil.GetProjectRoot(t)
		serviceName     = "prepare"
		cniConflistPath = "/etc/cni/net.d/test.conflist"
		cniBinPath      = "/opt/cni/bin"
		dockerconfig    = os.Getenv(prepareDockerConfigEnv)
		cniVersion      = "v0.9.1"
		cniURL          = fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz", cniVersion, runtime.GOARCH, cniVersion)
	)
	dockerConfigMount := ""
	if dockerconfig != "" {
		if !filepath.IsAbs(dockerconfig) {
			t.Fatalf("dockerconfig must be an absolute path")
		}
		dockerConfigMount = fmt.Sprintf(`    - "%s:/root/.docker/config.json:ro"`, dockerconfig)
	}
	c, err := compose.New(testutil.ApplyTextTemplate(t, `
version: "3.7"
services:
  {{.ServiceName}}:
    build:
      context: {{.ImageContextDir}}
      target: snapshotter-base
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
{{.DockerConfigMount}}

volumes:
  containerd-data:
  containerd-stargz-grpc-data:
`, struct {
		ServiceName       string
		ImageContextDir   string
		DockerConfigMount string
	}{
		ServiceName:       serviceName,
		ImageContextDir:   pRoot,
		DockerConfigMount: dockerConfigMount,
	}), compose.WithStdio(testutil.TestingLogDest()))
	if err != nil {
		t.Fatalf("failed to create compose: %v", err)
	}
	de, ok := c.Get(serviceName)
	if !ok {
		t.Fatalf("failed to get shell of service %v", serviceName)
	}
	sh := shell.New(de, testutil.NewTestingReporter(t))
	sh.
		X("apt-get", "update", "-y").
		X("apt-get", "--no-install-recommends", "install", "-y", "iptables").
		X("mkdir", "-p", cniBinPath).
		Pipe(nil, shell.C("curl", "-Ls", cniURL), shell.C("tar", "zxv", "-C", cniBinPath))
	cniConf := `
{
  "cniVersion": "0.4.0",
  "name": "test",
  "plugins" : [{
    "type": "bridge",
    "bridge": "test0",
    "isDefaultGateway": true,
    "forceAddress": false,
    "ipMasq": true,
    "hairpinMode": true,
    "ipam": {
      "type": "host-local",
      "subnet": "10.10.0.0/16"
    }
  },
  {
    "type": "loopback"
  }]
}
`
	if err := testutil.WriteFileContents(sh, cniConflistPath, []byte(cniConf), 0755); err != nil {
		t.Fatalf("failed to write cni config to %v: %v", cniConflistPath, err)
	}
	sh.
		X("update-alternatives", "--set", "iptables", "/usr/sbin/iptables-legacy").
		X("go", "get", "github.com/google/go-containerregistry/cmd/crane").
		Gox("containerd", "--log-level", "debug").
		Retry(100, "nerdctl", "version")
	return &Preparer{c, sh}
}

func (p *Preparer) Prepare(t *testing.T, name string, spec interface{}, mode types.Mode, targetRepo string) {
	var (
		src = fmt.Sprintf("%s/%s", srcRepo, name)
		dst = formatImageName(t, mode, targetRepo, name)
	)
	switch mode {
	case types.Legacy:
		p.sh.X("crane", "copy", src, dst)
	case types.EStargz:
		p.sh.X("nerdctl", "image", "pull", src)
		p.optimize(t, spec, src, dst)
		p.sh.X("nerdctl", "image", "push", dst)
	case types.EStargzNoopt:
		p.sh.X("nerdctl", "image", "pull", src)
		p.sh.X("ctr-remote", "i", "optimize", "--oci", "--no-optimize", src, dst)
		p.sh.X("nerdctl", "image", "push", dst)
	}
}

func (p *Preparer) Cleanup() error {
	return p.c.Cleanup()
}

func (p *Preparer) optimize(t *testing.T, spec interface{}, src, dst string) {
	switch v := spec.(type) {
	case types.BenchEchoHello:
		p.optimizeBenchEchoHello(t, v, src, dst)
	case types.BenchCmdArg:
		p.optimizeBenchCmdArg(t, v, src, dst)
	case types.BenchCmdArgWait:
		p.optimizeBenchCmdArgWait(t, v, src, dst)
	case types.BenchCmdStdin:
		p.optimizeBenchCmdStdin(t, v, src, dst)
	default:
		t.Fatalf("unknown type of spec: %T", v)
	}
}

func (p *Preparer) optimizeBenchCmdStdin(t *testing.T, spec types.BenchCmdStdin, src, dst string) {
	cmd := shell.C("ctr-remote", "i", "optimize", "-i", "--oci", "-cni", "-period", "30")
	for _, m := range spec.Mounts {
		srcInSh := "/mountsource" + xid.New().String()
		if err := testutil.CopyInDir(p.sh, m.Src, srcInSh); err != nil {
			t.Fatalf("failed copy mount dir: %v", err)
		}
		cmd = append(cmd, "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s,options=rbind",
			srcInSh, m.Dst))
	}
	if len(spec.Args) != 0 {
		args, err := json.Marshal(spec.Args)
		if err != nil {
			t.Fatalf("failed to encode args: %v", err)
		}
		cmd = append(cmd, "-args", string(args))
	}
	cmd = append(cmd, src, dst)
	shcmd := p.sh.Command(cmd[0], cmd[1:]...)
	shcmd.Stdin = bytes.NewReader([]byte(spec.Stdin))
	shcmd.Stdout = os.Stdout
	shcmd.Stderr = os.Stderr
	if err := shcmd.Run(); err != nil {
		t.Fatalf("failed to run %v: %v", cmd, err)
	}
}

func (p *Preparer) optimizeBenchCmdArgWait(t *testing.T, spec types.BenchCmdArgWait, src, dst string) {
	cmd := shell.C("ctr-remote", "i", "optimize", "--oci", "-cni", "-period", "30")
	if len(spec.Args) != 0 {
		args, err := json.Marshal(spec.Args)
		if err != nil {
			t.Fatalf("failed to encode args: %v", err)
		}
		cmd = append(cmd, "-args", string(args))
	}
	for _, e := range spec.Env {
		cmd = append(cmd, "-env", e)
	}
	cmd = append(cmd, src, dst)
	p.sh.X(cmd...)
}

func (p *Preparer) optimizeBenchCmdArg(t *testing.T, spec types.BenchCmdArg, src, dst string) {
	cmd := shell.C("ctr-remote", "i", "optimize", "--oci", "-cni", "-period", "30")
	if len(spec.Args) != 0 {
		args, err := json.Marshal(spec.Args)
		if err != nil {
			t.Fatalf("failed to encode args: %v", err)
		}
		cmd = append(cmd, "-args", string(args))
	}
	cmd = append(cmd, src, dst)
	p.sh.X(cmd...)
}

func (p *Preparer) optimizeBenchEchoHello(t *testing.T, spec interface{}, src, dst string) {
	entrypoint, err := json.Marshal([]string{"/bin/sh", "-c"})
	if err != nil {
		t.Fatalf("failed to encode entrypoint: %v", err)
	}
	args, err := json.Marshal([]string{"echo hello"})
	if err != nil {
		t.Fatalf("failed to encode args: %v", err)
	}
	p.sh.X("ctr-remote", "i", "optimize", "--oci", "-cni", "-period", "10",
		"-entrypoint", string(entrypoint), "-args", string(args), src, dst)
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
