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

package main

import (
	"context"
	"fmt"

	"github.com/containerd/stargz-snapshotter/cmd/docker-optimize/optimizer"
	"github.com/docker/cli/cli-plugins/manager"
	"github.com/docker/cli/cli-plugins/plugin"
	"github.com/docker/cli/cli/command"
	"github.com/spf13/cobra"
)

func main() {
	plugin.Run(func(dockerCli command.Cli) *cobra.Command {
		var cfg optimizer.OptimizeConfig
		cmd := &cobra.Command{
			Use:               "optimize",
			Short:             "Optimize an image with user-specified workload",
			PersistentPreRunE: plugin.PersistentPreRunE,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg.ConfigFile = dockerCli.ConfigFile().GetFilename()
				if len(args) != 2 {
					return fmt.Errorf("source and destination must be specified")
				}
				cfg.Src, cfg.Dst = args[0], args[1]
				return optimizer.Optimize(context.Background(), dockerCli, cfg)
			},
		}
		flags := cmd.Flags()
		flags.BoolVar(&cfg.PlainHTTP, "plain-http", false, "allow HTTP connections to the registry which has the prefix \"http://\"")
		flags.BoolVar(&cfg.StargzOnly, "stargz-only", false, "only stargzify and do not optimize layers")
		flags.BoolVarP(&cfg.Terminal, "terminal", "t", false, "enable terminal for sample container")
		flags.IntVar(&cfg.Period, "period", optimizer.DefaultPeriod, "time period to monitor access log")
		flags.StringVar(&cfg.User, "user", "", "user name to override image's default config")
		flags.StringVar(&cfg.Cwd, "cwd", "", "working dir to override image's default config")
		flags.StringVar(&cfg.Args, "args", "", "command arguments to override image's default config(in JSON array)")
		flags.StringVar(&cfg.Entrypoint, "entrypoint", "", "entrypoint to override image's default config(in JSON array)")
		flags.StringSliceVar(&cfg.Env, "env", nil, "environment valulable to add or override to the image's default config")
		return cmd
	}, manager.Metadata{
		SchemaVersion:    "0.1.0",
		Vendor:           "containerd/stargz-snapshotter",
		Version:          "0.0.0",
		ShortDescription: "Optimize an image with user-specified workload",
		URL:              "https://github.com/containerd/stargz-snapshotter",
	})
}
