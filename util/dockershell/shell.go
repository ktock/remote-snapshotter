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

package dockershell

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"golang.org/x/sync/errgroup"
)

// Supported checks if this pkg can run on the current system.
func Supported() error {
	return dexec.Supported()
}

// Reporter is used by Shell pkg to report logs and errors during commands execution.
type Reporter interface {

	// Errorf is called when Shell encounters unrecoverable error.
	Errorf(format string, v ...interface{})

	// Logf is called to report some useful information (e.g. executing command) by Shell.
	Logf(format string, v ...interface{})

	// Stdout is used as a stdout destination of executing commands.
	Stdout() io.Writer

	// Stdout is used as a stderr destination of executing commands.
	Stderr() io.Writer
}

// DefaultReporter is the default implementation of Reporter.
type DefaultReporter struct{}

// Errorf prints the occurred error.
func (r DefaultReporter) Errorf(format string, v ...interface{}) {
	fmt.Printf("error: %v\n", fmt.Sprintf(format, v...))
}

// Errorf prints the information reported.
func (r DefaultReporter) Logf(format string, v ...interface{}) {
	fmt.Printf("log: %v\n", fmt.Sprintf(format, v...))
}

// Stdout provides the writer to stdout.
func (r DefaultReporter) Stdout() io.Writer {
	return os.Stdout
}

// Stdout provides the writer to stderr.
func (r DefaultReporter) Stderr() io.Writer {
	return os.Stderr
}

// Shell provides provides means to execute commands inside a container, in
// a shellscript-like experience.
type Shell struct {
	*dexec.Exec
	r         Reporter
	err       error
	invalid   bool
	invalidMu sync.Mutex
}

// New creates a new Shell for the provided execution environment created by packages including
// dockershell/exec, dockershell/compose and dockershell/kind, etc.
//
// Most of methods of Shell don't return error but returns Shell itself. This allows the user to
// run commands using methods chain like Shell.X(commandA).X(commandB).X(commandC). This provides
// shellscript-like experience. Instead of reporting errors as return values, Shell reports errors
// through Reporter.

// When Shell encounters an unrecoverable error (e.g. failure of a command execution), this immediately
// calls Reporter.Errorf and don't execute the remaining (chained) commands. Err() method returns the
// last encountered error. Once Shell encounters an error, this is marked as "invalid" and doesn't
// accept any further command execution. For continuing further execution, call Refresh for aquiering
// a new instance of Shell.
//
// Some useful information are also reported via Reporter.Logf during commands execution and command
// outputs to stdio are streamed into Reporter.Stdout and Reporter.Stderr.
//
// If no Reporter is specified (i.e. nil is provided), DefaultReporter is used by default.
func New(de *dexec.Exec, r Reporter) *Shell {
	if r == nil {
		r = DefaultReporter{}
	}
	return &Shell{
		Exec: de,
		r:    r,
	}
}

func (s *Shell) fatal(format string, v ...interface{}) *Shell {
	s.r.Errorf(format, v...)
	s.err = fmt.Errorf(format, v...)
	s.invalidMu.Lock()
	s.invalid = true
	s.invalidMu.Unlock()
	return s
}

// Err returns an error encouterd at the last.
func (s *Shell) Err() error {
	return s.err
}

// IsInvalid returns true when this Shell is marked as "invalid". For continuing further
// command execution, call Refresh for aquiering a new instance of Shell.
func (s *Shell) IsInvalid() bool {
	s.invalidMu.Lock()
	b := s.invalid
	s.invalidMu.Unlock()
	return b
}

// Refresh returns a new cloned instance of this Shell.
func (s *Shell) Refresh() *Shell {
	return New(s.Exec, s.r)
}

// X executes a command. Stdio is streamed to Reporter. When the command fails, the error is reported
// via Reporter.Errorf and this Shell is marked as "invalid" (i.e. doesn't accept further command
// execution).
func (s *Shell) X(args ...string) *Shell {
	if s.IsInvalid() {
		return s
	}
	if len(args) < 1 {
		return s.fatal("no command to run")
	}
	s.r.Logf(">>> Running: %v\n", args)
	cmd := s.Command(args[0], args[1:]...)
	cmd.Stdout = s.r.Stdout()
	cmd.Stderr = s.r.Stderr()
	if err := cmd.Run(); err != nil {
		return s.fatal("failed to run %v: %v", args, err)
	}
	return s
}

// XLog executes a command. Stdio is streamed to Reporter. When the command fails, different from X,
// the error is reported via Reporter.Logf and this Shell still *accepts* further command execution.
func (s *Shell) XLog(args ...string) *Shell {
	if s.IsInvalid() {
		return s
	}
	if len(args) < 1 {
		return s.fatal("no command to run")
	}
	s.r.Logf(">>> Running: %v\n", args)
	cmd := s.Command(args[0], args[1:]...)
	cmd.Stdout = s.r.Stdout()
	cmd.Stderr = s.r.Stderr()
	if err := cmd.Run(); err != nil {
		s.r.Logf("failed to run %v: %v", args, err)
	}
	return s
}

// Gox executes a command in an goroutine and doesn't wait for the command completion. Stdio is
// streamed to Reporter. When the command fails, different from X, the error is reported via
// Reporter.Logf and this Shell still *accepts* further command execution.
func (s *Shell) Gox(args ...string) *Shell {
	if s.IsInvalid() {
		return s
	}
	if len(args) < 1 {
		return s.fatal("no command to run")
	}
	go func() {
		s.r.Logf(">>> Running: %v\n", args)
		cmd := s.Command(args[0], args[1:]...)
		cmd.Stdout = s.r.Stdout()
		cmd.Stderr = s.r.Stderr()
		if err := cmd.Run(); err != nil {
			s.r.Logf("command %v exit: %v", args, err)
		}
	}()
	return s
}

// C is an alias of []string which represents a command.
func C(args ...string) []string { return args }

// Pipe executes passed commands sequentially and stdout of a command is piped into the next command's
// stdin. The stdout of the last command is streamed to the specified io.Writer.
// When a command fails,  the error is reported via Reporter.Errorf and this Shell is marked as
// "invalid" (i.e. doesn't accept further command execution).
func (s *Shell) Pipe(out io.Writer, commands ...[]string) *Shell {
	if s.IsInvalid() {
		return s
	}
	if out == nil {
		out = s.r.Stdout()
	}
	var eg errgroup.Group
	var lastStdout io.ReadCloser
	for i, args := range commands {
		i, args := i, args
		if len(args) < 1 {
			return s.fatal("no command to run")
		}
		s.r.Logf(">>> Running: %v\n", args)
		cmd := s.Command(args[0], args[1:]...)
		cmd.Stdin = lastStdout
		pr, pw := io.Pipe()
		if i == len(commands)-1 {
			cmd.Stdout = out
		} else {
			cmd.Stdout = pw
			lastStdout = pr
		}
		cmd.Stderr = s.r.Stderr()
		eg.Go(func() error {
			if err := cmd.Run(); err != nil {
				pw.CloseWithError(err)
				return err
			}
			pw.Close()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return s.fatal("failed to run piped commands %v: %v", commands, err)
	}

	return s
}

// Retry executes a command repeatedly until it succeeds, up to num times. Stdio is streamed to
// Reporter. If all attemptions fail, the error is reported via Reporter.Errorf and this Shell is
// marked as "invalid" (i.e. doesn't accept further command execution).
func (s *Shell) Retry(num int, args ...string) *Shell {
	if s.IsInvalid() {
		return s
	}
	for i := 0; i < num; i++ {
		s.r.Logf(">>> Running(%d/%d): %v\n", i, num, args)
		cmd := s.Command(args[0], args[1:]...)
		cmd.Stdout = s.r.Stdout()
		cmd.Stderr = s.r.Stderr()
		err := cmd.Run()
		if err == nil {
			return s
		}
		s.r.Logf("failed to run (%d/%d) %v: %v", i, num, args, err)
		time.Sleep(time.Second)
	}
	return s.fatal("failed to run %v", args)
}

// O executes a command and return the stdout. Stderr is streamed to Reporter. When the command fails,
// the error is reported via Reporter.Errorf and this Shell is marked as "invalid" (i.e. doesn't
// accept further command execution).
func (s *Shell) O(args ...string) []byte {
	if s.IsInvalid() {
		return nil
	}
	if len(args) < 1 {
		s.fatal("no command to run")
		return nil
	}
	s.r.Logf(">>> Getting output of: %v\n", args)
	cmd := s.Command(args[0], args[1:]...)
	cmd.Stderr = s.r.Stderr()
	out, err := cmd.Output()
	if err != nil {
		s.fatal("failed to run for getting output from %v: %v", args, err)
		return nil
	}
	return out
}

// R executes a command. Stdio is returned as io.Reader. streamed to Reporter.
func (s *Shell) R(args ...string) (stdout, stderr io.Reader, err error) {
	if s.IsInvalid() {
		return nil, nil, fmt.Errorf("invalid shell")
	}
	if len(args) < 1 {
		return nil, nil, fmt.Errorf("no command to run")
	}
	s.r.Logf(">>> Running(returning reader): %v\n", args)
	cmd := s.Command(args[0], args[1:]...)
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout, cmd.Stderr = outW, errW
	go func() {
		if err := cmd.Run(); err != nil {
			outW.CloseWithError(err)
			errW.CloseWithError(err)
			return
		}
		outW.Close()
		errW.Close()
	}()
	return outR, errR, nil
}

// ForEach executes a command. For each line of stdout, the callback function is called until it
// returns false. Stderr is streamed to Reporter. The encountered erros are returned instead of
// using Reporter.
func (s *Shell) ForEach(args []string, f func(l string) bool) error {
	stdout, stderr, err := s.R(args...)
	if err != nil {
		return err
	}
	go io.Copy(s.r.Stderr(), stderr)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if !f(scanner.Text()) {
			break
		}
	}
	return nil
}
