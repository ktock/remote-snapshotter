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

package compose

import (
	"bufio"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/xid"
)

// Supported checks if this pkg can run on the current system.
func Supported() error {
	if err := exec.Command("docker", "version").Run(); err != nil {
		return err
	}
	return exec.Command("docker-compose", "--version").Run()
}

// Compose represents a set of container execution environment (i.e. a set of *dexec.Exec) that
// is orchestrated as a docker compose project.
// This can be created using docker compose yaml. Get method provides *dexec.Exec
// of arbitrary service.
type Compose struct {
	execs    map[string]*dexec.Exec
	cleanups []func() error
}

type options struct {
	buildArgs []string
	addStdio  func(c *exec.Cmd)
	addStderr func(c *exec.Cmd)
}

// Option is an option for creating compose.
type Option func(o *options)

// WithBuildArgs specifies the build args that will be used during build.
func WithBuildArgs(buildArgs ...string) Option {
	return func(o *options) {
		o.buildArgs = buildArgs
	}
}

// WithStdio specifies stdio which docker-compose build command's stdio will be streamed into.
func WithStdio(stdout, stderr io.Writer) Option {
	return func(o *options) {
		o.addStdio = func(c *exec.Cmd) {
			c.Stdout = stdout
			c.Stderr = stderr
		}
		o.addStderr = func(c *exec.Cmd) {
			c.Stderr = stderr
		}
	}
}

// New creates a new Compose of the specified docker-compose yaml data.
func New(dockerComposeYaml string, opts ...Option) (*Compose, error) {
	var cOpts options
	for _, o := range opts {
		o(&cOpts)
	}
	tmpContext, err := ioutil.TempDir("", "compose"+xid.New().String())
	if err != nil {
		return nil, err
	}
	confFile := filepath.Join(tmpContext, "docker-compose.yml")
	if err := ioutil.WriteFile(confFile, []byte(dockerComposeYaml), 0666); err != nil {
		return nil, err
	}

	var cleanups []func() error
	cleanups = append(cleanups, func() error {
		return exec.Command("docker-compose", "-f", confFile, "down", "-v").Run()
	})
	cleanups = append(cleanups, func() error { return os.RemoveAll(tmpContext) })

	var buildArgs []string
	for _, arg := range cOpts.buildArgs {
		buildArgs = append(buildArgs, "--build-arg", arg)
	}
	cmd := exec.Command("docker-compose", append([]string{"-f", confFile, "build"}, buildArgs...)...)
	if cOpts.addStdio != nil {
		cOpts.addStdio(cmd)
	}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	cmd = exec.Command("docker-compose", "-f", confFile, "up", "-d")
	if cOpts.addStdio != nil {
		cOpts.addStdio(cmd)
	}
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	cmd = exec.Command("docker-compose", "-f", confFile, "ps", "--services")
	if cOpts.addStderr != nil {
		cOpts.addStderr(cmd)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var services []string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		services = append(services, strings.TrimSpace(scanner.Text()))
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	execs := map[string]*dexec.Exec{}
	for _, s := range services {
		cmd = exec.Command("docker-compose", "-f", confFile, "ps", "-q", s)
		if cOpts.addStderr != nil {
			cOpts.addStderr(cmd)
		}
		cNameB, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		de, err := dexec.New(strings.TrimSpace(string(cNameB)))
		if err != nil {
			return nil, err
		}
		execs[s] = de
	}

	return &Compose{execs, cleanups}, nil
}

// Get returns *dexec.Exec of an arbitrary service contained in this Compose.
func (c *Compose) Get(serviceName string) (*dexec.Exec, bool) {
	v, ok := c.execs[serviceName]
	return v, ok
}

// List lists all service names contained in this Compose.
func (c *Compose) List() (l []string) {
	for k := range c.execs {
		l = append(l, k)
	}
	return
}

// Cleanup teardowns this Compose and cleans up related resources.
func (c *Compose) Cleanup() (retErr error) {
	for _, f := range c.cleanups {
		if err := f(); err != nil {
			retErr = multierror.Append(retErr, err)
		}
	}
	return
}
