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
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rs/xid"
)

// NewTempNetwork creates a new network and returns its cleaner function.
func NewTempNetwork(networkName string) (func() error, error) {
	cmd := exec.Command("docker", "network", "create", networkName)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return func() error {
		return exec.Command("docker", "network", "rm", networkName).Run()
	}, nil
}

// Connect connects an Exec to the specified docker network.
func Connect(de *Exec, networkName string) error {
	return exec.Command("docker", "network", "connect", networkName, de.ContainerName).Run()
}

type imageOptions struct {
	patchDockerfile string
	patchContextDir string
	buildArgs       []string
	addStdio        func(c *exec.Cmd)
}

type ImageOption func(o *imageOptions)

// WithPatchDockerfile is a part of Dockerfile that will be built based on the
// Dockerfile specified by the arguments of NewTempImage.
func WithPatchDockerfile(patchDockerfile string) ImageOption {
	return func(o *imageOptions) {
		o.patchDockerfile = patchDockerfile
	}
}

// WithPatchContextDir is a context dir of a build which will be executed based on the
// Dockerfile specified by the arguments of NewTempImage. When this option is used,
// WithPatchDockerfile corresponding to this context dir must be specified as well.
func WithPatchContextDir(patchContextDir string) ImageOption {
	return func(o *imageOptions) {
		o.patchContextDir = patchContextDir
	}
}

// WithTempImageBuildArgs specifies the build args that will be used during build.
func WithTempImageBuildArgs(buildArgs ...string) ImageOption {
	return func(o *imageOptions) {
		o.buildArgs = buildArgs
	}
}

// WithTempImageStdio specifies stdio which docker build command's stdio will be streamed into.
func WithTempImageStdio(stdout, stderr io.Writer) ImageOption {
	return func(o *imageOptions) {
		o.addStdio = func(c *exec.Cmd) {
			c.Stdout = stdout
			c.Stderr = stderr
		}
	}
}

// NewTempImage builds a new image of the specified context and stage then returns the tag and
// cleaner function.
func NewTempImage(contextDir, targetStage string, opts ...ImageOption) (string, func() error, error) {
	var iOpts imageOptions
	for _, o := range opts {
		o(&iOpts)
	}
	if iOpts.patchContextDir != "" {
		if iOpts.patchDockerfile == "" {
			return "", nil, fmt.Errorf("Dockerfile patch must be specified with context dir")
		}
	}
	if !filepath.IsAbs(contextDir) {
		return "", nil, fmt.Errorf("context dir %v must be an absolute path", contextDir)
	}

	tmpImage, tmpDone, err := newTempImage(contextDir, "", targetStage, &iOpts)
	if err != nil {
		return "", nil, err
	}
	if iOpts.patchDockerfile == "" {
		return tmpImage, tmpDone, err
	}
	defer tmpDone()

	patchContextDir := iOpts.patchContextDir
	if patchContextDir == "" {
		patchContextDir, err = ioutil.TempDir("", "tmpcontext")
		if err != nil {
			return "", nil, err
		}
		defer os.RemoveAll(patchContextDir)
	}
	dfData := fmt.Sprintf(`
FROM %s

%s
`, tmpImage, iOpts.patchDockerfile)
	dfContextDir, err := ioutil.TempDir("", "tmpdfcontext")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dfContextDir)
	dockerfilePath := filepath.Join(dfContextDir, "Dockerfile")
	if err := ioutil.WriteFile(dockerfilePath, []byte(dfData), 0666); err != nil {
		return "", nil, err
	}
	return newTempImage(patchContextDir, dockerfilePath, "", &iOpts)
}

func newTempImage(contextDir, dockerfilePath, targetStage string, opts *imageOptions) (string, func() error, error) {
	image := "tmpimage" + xid.New().String()
	c := []string{"build", "--progress", "plain", "-t", image}
	if dockerfilePath != "" {
		c = append(c, "-f", dockerfilePath)
	}
	if targetStage != "" {
		c = append(c, "--target", targetStage)
	}
	for _, arg := range opts.buildArgs {
		c = append(c, "--build-arg", arg)
	}
	c = append(c, contextDir)
	cmd := exec.Command("docker", c...)
	if opts.addStdio != nil {
		opts.addStdio(cmd)
	}
	if err := cmd.Run(); err != nil {
		return "", nil, err
	}
	return image, func() error {
		cmd := exec.Command("docker", "image", "rm", image)
		if opts.addStdio != nil {
			opts.addStdio(cmd)
		}
		return cmd.Run()
	}, nil
}
