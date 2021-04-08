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

package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/zstdchunked"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var GetTOCDigestCommand = cli.Command{
	Name:      "gettocdigest",
	Usage:     "get the digest of TOC of a layer",
	ArgsUsage: "<layer digest>",
	Flags: []cli.Flag{
		// zstd:chunked flags
		cli.BoolFlag{
			Name:  "zstdchunked",
			Usage: "parse layer as zstd:chunked",
		},
		// other flags for debugging
		cli.BoolFlag{
			Name:  "dump-toc",
			Usage: "dump TOC instead of digest. Note that the dumped TOC might be formatted with indents so may have different digest against the original in the layer",
		},
	},
	Action: func(clicontext *cli.Context) error {
		layerDgstStr := clicontext.Args().Get(0)
		if layerDgstStr == "" {
			return errors.New("layer digest need to be specified")
		}

		client, ctx, cancel, err := commands.NewClient(clicontext)
		if err != nil {
			return err
		}
		defer cancel()

		layerDgst, err := digest.Parse(layerDgstStr)
		if err != nil {
			return err
		}
		ra, err := client.ContentStore().ReaderAt(ctx, ocispec.Descriptor{Digest: layerDgst})
		if err != nil {
			return err
		}
		defer ra.Close()

		footerSize := estargz.FooterSize
		if clicontext.Bool("zstdchunked") {
			footerSize = zstdchunked.FooterSize
		}
		footer := make([]byte, footerSize)
		if _, err := ra.ReadAt(footer, ra.Size()-int64(footerSize)); err != nil {
			return errors.Wrapf(err, "error reading footer")
		}

		var decompressor estargz.Decompressor
		decompressor = new(estargz.GzipDecompressor)
		if clicontext.Bool("zstdchunked") {
			decompressor = new(zstdchunked.Decompressor)
		}

		tocOff, tocSize, err := decompressor.ParseFooter(footer)
		if err != nil {
			return errors.Wrapf(err, "error parsing footer")
		}
		if tocSize <= 0 {
			tocSize = ra.Size() - tocOff - int64(footerSize)
		}
		toc, tocDgst, err := decompressor.ParseTOC(io.NewSectionReader(ra, tocOff, tocSize))
		if err != nil {
			return errors.Wrapf(err, "error parsing TOC")
		}

		if clicontext.Bool("dump-toc") {
			tocJSON, err := json.MarshalIndent(toc, "", "\t")
			if err != nil {
				return errors.Wrapf(err, "failed to marshal toc")
			}
			fmt.Println(string(tocJSON))
			return nil
		}
		fmt.Println(tocDgst.String())
		return nil
	},
}
