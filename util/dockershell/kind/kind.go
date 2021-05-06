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

package kind

import (
	"bufio"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
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
	if err := exec.Command("kubectl", "--help").Run(); err != nil {
		return err
	}
	return exec.Command("kind", "version").Run()
}

// Kind reperesents a set of container execution environment (i.e. set of *dexec.Exec) that
// is orchestrated as a Kind cluster.
// This can be created using Kind config yaml. Get method provides *dexec.Exec
// of arbitrary service. KubeCtl method provides the way to execute kubectl commands
// against the Kind cluster.
type Kind struct {
	execs          map[string]*dexec.Exec
	cleanups       []func() error
	kubeconfigPath string
}

type options struct {
	addStdio  func(c *exec.Cmd)
	addStderr func(c *exec.Cmd)
}

// Option is an option for creating Kind cluster.
type Option func(o *options)

// WithStdio specifies stdio which stdio of kind and kubectl commands will be streamed into.
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

// New creates a new Kind of the specified kind config yaml data.
func New(kindYaml string, opts ...Option) (*Kind, error) {
	var cleanups []func() error
	var kOpts options
	for _, o := range opts {
		o(&kOpts)
	}
	conf, err := ioutil.TempFile("", "tmpKindYaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(conf.Name())
	if _, err := conf.Write([]byte(kindYaml)); err != nil {
		return nil, err
	}
	if err := conf.Close(); err != nil {
		return nil, err
	}

	kc, err := ioutil.TempFile("", "tmpKindKC")
	if err != nil {
		return nil, err
	}
	kubeconfigPath := kc.Name()
	cleanups = append(cleanups, func() error { return os.Remove(kubeconfigPath) })
	defer kc.Close()

	clusterName := "kindcluster" + xid.New().String()
	cmd := exec.Command("kind", "create", "cluster",
		"--name", clusterName, "--kubeconfig", kubeconfigPath, "--config", conf.Name())
	if kOpts.addStdio != nil {
		kOpts.addStdio(cmd)
	}
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	cmd = exec.Command("kind", "get", "nodes", "--name", clusterName)
	if kOpts.addStderr != nil {
		kOpts.addStderr(cmd)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var nodes []string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		nodes = append(nodes, strings.TrimSpace(scanner.Text()))
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	execs := map[string]*dexec.Exec{}
	for _, c := range nodes {
		de, err := dexec.New(c)
		if err != nil {
			return nil, err
		}
		execs[c] = de
	}

	cleanups = append(cleanups, func() error {
		cmd = exec.Command("kind", "delete", "cluster", "--name", clusterName)
		if kOpts.addStdio != nil {
			kOpts.addStdio(cmd)
		}
		return cmd.Run()
	})
	return &Kind{
		execs:          execs,
		cleanups:       cleanups,
		kubeconfigPath: kubeconfigPath,
	}, nil
}

// KubeCtl executes kubectl command with the specified args against this Kind cluster.
func (k *Kind) KubeCtl(args ...string) *exec.Cmd {
	cmd := exec.Command("kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.kubeconfigPath)
	return cmd
}

// Get returns *dexec.Exec of an arbitrary node container in this Kind cluster.
func (k *Kind) Get(name string) (*dexec.Exec, bool) {
	v, ok := k.execs[name]
	return v, ok
}

// List lists all node names contained in this Kind cluster.
func (k *Kind) List() (l []string) {
	for k := range k.execs {
		l = append(l, k)
	}
	return
}

// Cleanup teardowns this Kind cluster and cleans up related resources.
func (k *Kind) Cleanup() (retErr error) {
	for _, f := range k.cleanups {
		if err := f(); err != nil {
			retErr = multierror.Append(retErr, err)
		}
	}
	return
}
