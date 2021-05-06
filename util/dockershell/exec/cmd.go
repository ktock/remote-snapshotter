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

package exec

import (
	"io"
	"os/exec"

	"github.com/pkg/errors"
)

// Supported checks if this pkg can run on the current system.
func Supported() error {
	return exec.Command("docker", "version").Run()
}

// Exec is an executing environment for a container. Commands can be executed in the
// container using Command method.
type Exec struct {

	// ContainerName is the name of the target container.
	ContainerName string
}

// New creates a new Exec for the specified container.
func New(containerName string) (*Exec, error) {
	if err := exec.Command("docker", "inspect", containerName).Run(); err != nil {
		return nil, errors.Wrapf(err, "container %v is unavailable", containerName)
	}
	return &Exec{containerName}, nil
}

// Command creates a new Cmd for the specified commands.
func (e Exec) Command(name string, arg ...string) *Cmd {
	cmd := &Cmd{
		Path:          name,
		Args:          append([]string{name}, arg...),
		dockerExec:    &exec.Cmd{},
		containerName: e.ContainerName,
	}
	if lp, err := exec.LookPath("docker"); err != nil {
		cmd.lookPathErr = errors.Wrap(err, "docker command not found")
	} else {
		cmd.dockerExec.Path = lp
	}
	return cmd
}

// Kill kills the underlying container.
func (e Exec) Kill() error {
	return exec.Command("docker", "kill", e.ContainerName).Run()
}

// Cmd is exec.Cmd-like object which provides the way to execute commands in a container.
type Cmd struct {

	// Path is the path of the command to run.
	Path string

	// Args holds the command line arguents.
	Args []string

	// Env holds the environment variables for the command.
	Env []string

	// Dir specifies the working direcotroy of the command.
	Dir string

	// Stdin specifies the stdin of the command.
	Stdin io.Reader

	// Stdout and Stderr specifies the stdout and stderr of the command.
	Stdout io.Writer
	Stderr io.Writer

	lookPathErr   error
	dockerExec    *exec.Cmd
	containerName string

	// TODO: support the following fields
	// ExtraFiles []*os.File
	// SysProcAttr *syscall.SysProcAttr
	// Process *os.Process
	// ProcessState *os.ProcessState
}

func (cmd *Cmd) toDocker() *exec.Cmd {
	var opts []string
	if cmd.Stdin != nil {
		opts = append(opts, "-i")
	}
	if cmd.Dir != "" {
		opts = append(opts, "-w", cmd.Dir)
	}
	for _, e := range cmd.Env {
		opts = append(opts, "-e", e)
	}
	base := append([]string{"docker", "exec"}, append(opts, cmd.containerName)...)
	cmd.dockerExec.Args = append(base, cmd.Args...)
	cmd.dockerExec.Stdin = cmd.Stdin
	cmd.dockerExec.Stdout = cmd.Stdout
	cmd.dockerExec.Stderr = cmd.Stderr
	return cmd.dockerExec
}

// CombinedOutput runs the specified commands and returns the combined output of stdout and stderr.
func (cmd *Cmd) CombinedOutput() ([]byte, error) {
	if err := cmd.lookPathErr; err != nil {
		return nil, err
	}
	return cmd.toDocker().CombinedOutput()
}

// Output runs the specified commands and returns its stdout.
func (cmd *Cmd) Output() ([]byte, error) {
	if err := cmd.lookPathErr; err != nil {
		return nil, err
	}
	return cmd.toDocker().Output()
}

// Run runs the specified commands.
func (cmd *Cmd) Run() error {
	if err := cmd.lookPathErr; err != nil {
		return err
	}
	return cmd.toDocker().Run()
}

// StderrPipe returns the pipe that will be connected to stderr of the executed command.
func (cmd *Cmd) StderrPipe() (io.ReadCloser, error) {
	return cmd.toDocker().StderrPipe()
}

// StdinPipe returns the pipe that will be connected to stdin of the executed command.
func (cmd *Cmd) StdinPipe() (io.WriteCloser, error) {
	return cmd.toDocker().StdinPipe()
}

// StdoutPipe returns the pipe that will be connected to stdout of the executed command.
func (cmd *Cmd) StdoutPipe() (io.ReadCloser, error) {
	return cmd.toDocker().StdoutPipe()
}

// String returns a human-readable description of this command.
func (cmd *Cmd) String() string {
	return cmd.toDocker().String()
}
