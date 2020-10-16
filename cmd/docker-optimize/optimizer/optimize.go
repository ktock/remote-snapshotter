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

package optimizer

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"sync"

	"github.com/docker/cli/cli"
	"github.com/docker/cli/cli/command"
	cliContainer "github.com/docker/cli/cli/command/container"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
)

const (
	imageName     = "stargz/oind:202009"
	DefaultPeriod = 10
)

type OptimizeConfig struct {
	Src        string
	Dst        string
	PlainHTTP  bool
	StargzOnly bool
	Terminal   bool
	Period     int
	User       string
	Cwd        string
	Args       string
	Entrypoint string
	Env        []string
	ConfigFile string
}

// Optimize runs the optimization command using given Docker client.
func Optimize(ctx context.Context, dockerCli command.Cli, cfg OptimizeConfig) error {

	// Convert configuration
	cc, hc, err := convertConfig(cfg)
	if err != nil {
		return errors.Wrapf(err, "failed to generate config")
	}

	// Run and attach to the container
	return runAndAttach(ctx, dockerCli, cc, hc)
}

// convertConfig converts OptimizeConfig into the container's runtime
// configuration.
func convertConfig(cfg OptimizeConfig) (container.Config, container.HostConfig, error) {
	if cfg.Src == "" || cfg.Dst == "" {
		return container.Config{}, container.HostConfig{}, fmt.Errorf("both of source and destination must be specified")
	}

	var args []string
	if cfg.PlainHTTP {
		args = append(args, "--plain-http")
	}
	if cfg.StargzOnly {
		args = append(args, "--stargz-only")
	}
	if cfg.Terminal {
		args = append(args, "--terminal")
	}
	args = append(args, []string{
		"--period=" + strconv.Itoa(cfg.Period),
		"--user", cfg.User,
		"--cwd", cfg.Cwd,
		"--args", cfg.Args,
		"--entrypoint", cfg.Entrypoint,
	}...)
	for _, e := range cfg.Env {
		args = append(args, "--env", e)
	}
	args = append(args, cfg.Src, cfg.Dst)

	var binds []string
	if cfg.ConfigFile != "" {
		binds = append(binds, cfg.ConfigFile+":/root/.docker/config.json:ro")
	}

	return container.Config{
			Image:        imageName,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
			OpenStdin:    true,
			StdinOnce:    true,
			Cmd:          args,
		}, container.HostConfig{
			AutoRemove: true,
			Privileged: true,
			Tmpfs:      map[string]string{"/tmp": "exec,mode=777"},
			Binds:      binds,
			Resources: container.Resources{
				Devices: []container.DeviceMapping{
					{
						PathOnHost:      "/dev/fuse",
						PathInContainer: "/dev/fuse",
					},
				},
			},
		}, nil
}

// runAndAttach runs a container based on the config and attachs to it.
func runAndAttach(ctx context.Context, dockerCli command.Cli, cc container.Config, hc container.HostConfig) error {

	// Prepare command image
	rc, err := dockerCli.Client().ImageCreate(ctx, cc.Image, types.ImageCreateOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to pull image w%q", cc.Image)
	}
	defer rc.Close()
	if _, err := io.Copy(ioutil.Discard, rc); err != nil {
		return err
	}

	// Create command container
	cResp, err := dockerCli.Client().ContainerCreate(ctx, &cc, &hc, nil, nil, "")
	if err != nil {
		return errors.Wrapf(err, "failed to create container from %q", cc.Image)
	}

	// Attach to the command container
	aResp, err := dockerCli.Client().ContainerAttach(ctx, cResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return errors.Wrapf(err, "failed to attach to the container %q", cResp.ID)
	}
	defer aResp.Close()
	waitStream, doneStream, err := stream(dockerCli, aResp)
	if err != nil {
		return errors.Wrapf(err, "failed to setup stream")
	}
	defer doneStream()

	// Start the command container
	if err := dockerCli.Client().ContainerStart(ctx, cResp.ID, types.ContainerStartOptions{}); err != nil {
		return errors.Wrapf(err, "failed to start container %q", cResp.ID)
	}
	if dockerCli.Out().IsTerminal() {
		if err := cliContainer.MonitorTtySize(ctx, dockerCli, cResp.ID, false); err != nil {
			return errors.Wrapf(err, "cannot monitor tty size")
		}
	}

	// Wait for the command exit
	resultChan, errChan := dockerCli.Client().ContainerWait(ctx, cResp.ID, container.WaitConditionNextExit)
	if err := waitStream(ctx); err != nil {
		return errors.Wrapf(err, "stream error")
	}
	select {
	case result := <-resultChan:
		if result.Error != nil {
			return err
		}
		if result.StatusCode != 0 {
			return cli.StatusError{StatusCode: int(result.StatusCode)}
		}
	case err := <-errChan:
		return err
	}

	return nil
}

// stream copies hijacked response to/from docker cli.
func stream(dockerCli command.Cli, hijacked types.HijackedResponse) (wait func(context.Context) error, done func(), err error) {
	var (
		inDone, outDone = make(chan error), make(chan error)
		doneOnce        sync.Once
	)

	// Set the client's terminal mode into raw
	// for enabling to send control keys to the container
	if err := dockerCli.In().SetRawTerminal(); err != nil {
		return nil, nil, err
	}
	if err := dockerCli.Out().SetRawTerminal(); err != nil {
		return nil, nil, err
	}
	done = func() {
		// Restore the terminal mode when done
		doneOnce.Do(func() {
			dockerCli.In().RestoreTerminal()
			dockerCli.Out().RestoreTerminal()
		})
	}

	// Stream the hijacked response to/from docker cli
	go func() {
		_, err := io.Copy(hijacked.Conn, dockerCli.In())
		done() // Restore the terminal mode
		inDone <- err
	}()
	go func() {
		_, err := io.Copy(dockerCli.Out(), hijacked.Reader)
		done() // Restore the terminal mode
		outDone <- err
	}()

	return func(ctx context.Context) error {
		select {
		case err := <-outDone:
			return err
		case err := <-inDone:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}, done, nil
}
