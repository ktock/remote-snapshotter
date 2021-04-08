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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/analyzer"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/nativeconverter"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	zstdchunkedconvert "github.com/containerd/stargz-snapshotter/nativeconverter/zstdchunked"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const defaultPeriod = 10

var OptimizeCommand = cli.Command{
	Name:      "optimize",
	Usage:     "optimize an image with user-specified workload",
	ArgsUsage: "[flags] <source_ref> <target_ref>...",
	Flags: append([]cli.Flag{
		cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
		},
		cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: &cli.StringSlice{},
		},
		cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "targeting all platform of the source image",
		},
		cli.BoolFlag{
			Name:  "wait-on-signal",
			Usage: "ignore context cancel and keep the container running until it receives SIGINT (Ctrl + C) sent manually",
		},
		cli.BoolFlag{
			Name:  "no-optimize",
			Usage: "convert image without optimization",
		},
		cli.StringFlag{
			Name:  "record-out",
			Usage: "record the monitor log to the specified file",
		},
		cli.BoolFlag{
			Name:  "oci",
			Usage: "convert Docker media types to OCI media types",
		},
		cli.BoolFlag{
			Name:  "zstdchunked",
			Usage: "use zstd compression instead of gzip (a.k.a zstd:chunked)",
		},
	}, samplerFlags...),
	Action: func(clicontext *cli.Context) error {
		convertOpts := []converter.Opt{}
		srcRef := clicontext.Args().Get(0)
		targetRef := clicontext.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		var platformMC platforms.MatchComparer
		if clicontext.Bool("all-platforms") {
			platformMC = platforms.All
		} else {
			if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
				var all []ocispec.Platform
				for _, ps := range pss {
					p, err := platforms.Parse(ps)
					if err != nil {
						return errors.Wrapf(err, "invalid platform %q", ps)
					}
					all = append(all, p)
				}
				platformMC = platforms.Ordered(all...)
			} else {
				platformMC = platforms.DefaultStrict()
			}
		}
		convertOpts = append(convertOpts, converter.WithPlatform(platformMC))

		var docker2oci bool
		if clicontext.Bool("oci") {
			docker2oci = true
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		} else {
			if clicontext.Bool("zstdchunked") {
				return errors.New("option --zstdchunked must be used in conjunction with --oci")
			}
			logrus.Warn("option --oci should be used as well")
		}

		client, ctx, cancel, err := commands.NewClient(clicontext)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		recordOut, esgzOptsPerLayer, wrapper, err := analyze(ctx, clicontext, client, srcRef)
		if err != nil {
			return err
		}
		if recordOutFile := clicontext.String("record-out"); recordOutFile != "" {
			if err := writeContentFile(ctx, client, recordOut, recordOutFile); err != nil {
				return errors.Wrapf(err, "failed output record file")
			}
		}
		var f converter.ConvertFunc
		if clicontext.Bool("zstdchunked") {
			f = zstdchunkedconvert.LayerConvertWithLayerOptsFunc(esgzOptsPerLayer)
		} else {
			f = estargzconvert.LayerConvertWithLayerOptsFunc(esgzOptsPerLayer)
		}
		if wrapper != nil {
			f = wrapper(f)
		}
		layerConvertFunc := logWrapper(f)
		convertOpts = append(convertOpts, converter.WithLayerConvertFunc(layerConvertFunc))
		convertOpts = append(convertOpts, converter.WithIndexConvertFunc(
			// index converter patched for zstd compression
			// TODO: upstream this to containerd/containerd
			nativeconverter.IndexConvertFunc(layerConvertFunc, docker2oci, platformMC)))
		newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		fmt.Fprintln(clicontext.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

func writeContentFile(ctx context.Context, client *containerd.Client, dgst digest.Digest, targetFile string) error {
	fw, err := os.Create(targetFile)
	if err != nil {
		return err
	}
	defer fw.Close()
	ra, err := client.ContentStore().ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return err
	}
	defer ra.Close()
	_, err = io.Copy(fw, io.NewSectionReader(ra, 0, ra.Size()))
	return err
}

func analyze(ctx context.Context, clicontext *cli.Context, client *containerd.Client, srcRef string) (digest.Digest, map[digest.Digest][]estargz.Option, func(converter.ConvertFunc) converter.ConvertFunc, error) {
	if clicontext.Bool("no-optimize") {
		return "", nil, nil, nil
	}

	// Do analysis only when the target platforms contain the current platform
	if !clicontext.Bool("all-platforms") {
		if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
			containsDefault := false
			for _, ps := range pss {
				p, err := platforms.Parse(ps)
				if err != nil {
					return "", nil, nil, errors.Wrapf(err, "invalid platform %q", ps)
				}
				if platforms.DefaultStrict().Match(p) {
					containsDefault = true
				}
			}
			if !containsDefault {
				return "", nil, nil, nil // do not run analyzer
			}
		}
	}

	cs := client.ContentStore()
	is := client.ImageService()

	// Analyze layers and get prioritized files
	aOpts := []analyzer.Option{analyzer.WithSpecOpts(getSpecOpts(clicontext))}
	if clicontext.Bool("wait-on-signal") && clicontext.Bool("terminal") {
		return "", nil, nil, fmt.Errorf("wait-on-signal can't be used with terminal flag")
	}
	if clicontext.Bool("wait-on-signal") {
		aOpts = append(aOpts, analyzer.WithWaitOnSignal())
	} else {
		aOpts = append(aOpts,
			analyzer.WithPeriod(time.Duration(clicontext.Int("period"))*time.Second))
	}
	if clicontext.Bool("terminal") {
		if !clicontext.Bool("i") {
			return "", nil, nil, fmt.Errorf("terminal flag must be specified with \"-i\"")
		}
		aOpts = append(aOpts, analyzer.WithTerminal())
	}
	if clicontext.Bool("i") {
		aOpts = append(aOpts, analyzer.WithStdin())
	}
	recordOut, err := analyzer.Analyze(ctx, client, srcRef, aOpts...)
	if err != nil {
		return "", nil, nil, err
	}

	// Parse record file
	srcImg, err := is.Get(ctx, srcRef)
	if err != nil {
		return "", nil, nil, err
	}
	manifestDesc, err := containerdutil.ManifestDesc(ctx, cs, srcImg.Target, platforms.DefaultStrict())
	if err != nil {
		return "", nil, nil, err
	}
	p, err := content.ReadBlob(ctx, cs, manifestDesc)
	if err != nil {
		return "", nil, nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return "", nil, nil, err
	}
	// TODO: this should be indexed by layer "index" (not "digest")
	layerLogs := make(map[digest.Digest][]string, len(manifest.Layers))
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: recordOut})
	if err != nil {
		return "", nil, nil, err
	}
	defer ra.Close()
	dec := json.NewDecoder(io.NewSectionReader(ra, 0, ra.Size()))
	added := make(map[digest.Digest]map[string]struct{}, len(manifest.Layers))
	for dec.More() {
		var e recorder.Entry
		if err := dec.Decode(&e); err != nil {
			return "", nil, nil, err
		}
		if *e.LayerIndex < len(manifest.Layers) &&
			e.ManifestDigest == manifestDesc.Digest.String() {
			dgst := manifest.Layers[*e.LayerIndex].Digest
			if added[dgst] == nil {
				added[dgst] = map[string]struct{}{}
			}
			if _, ok := added[dgst][e.Path]; !ok {
				added[dgst][e.Path] = struct{}{}
				layerLogs[dgst] = append(layerLogs[dgst], e.Path)
			}
		}
	}

	// Create a converter wrapper for skipping layer conversion. This skip occurs
	// if "reuse" option is specified, the source layer is already valid estargz
	// and no access occur to that layer.
	var excludes []digest.Digest
	layerOpts := make(map[digest.Digest][]estargz.Option, len(manifest.Layers))
	for _, desc := range manifest.Layers {
		if layerLog, ok := layerLogs[desc.Digest]; ok && len(layerLog) > 0 {
			layerOpts[desc.Digest] = []estargz.Option{estargz.WithPrioritizedFiles(layerLog)}
		} else if clicontext.Bool("reuse") && isReusableESGZLayer(ctx, desc, cs) {
			excludes = append(excludes, desc.Digest) // reuse layer without conversion
		}
	}
	return recordOut, layerOpts, excludeWrapper(excludes), nil
}

func isReusableESGZLayer(ctx context.Context, desc ocispec.Descriptor, cs content.Store) bool {
	dgstStr, ok := desc.Annotations[estargz.TOCJSONDigestAnnotation]
	if !ok {
		return false
	}
	tocdgst, err := digest.Parse(dgstStr)
	if err != nil {
		return false
	}
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return false
	}
	defer ra.Close()
	r, err := estargz.Open(io.NewSectionReader(ra, 0, desc.Size))
	if err != nil {
		return false
	}
	if _, err := r.VerifyTOC(tocdgst); err != nil {
		return false
	}
	return true
}

func excludeWrapper(excludes []digest.Digest) func(converter.ConvertFunc) converter.ConvertFunc {
	return func(convertFunc converter.ConvertFunc) converter.ConvertFunc {
		return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
			for _, e := range excludes {
				if e == desc.Digest {
					logrus.Warnf("reusing %q without conversion", e)
					return nil, nil
				}
			}
			return convertFunc(ctx, cs, desc)
		}
	}
}

func logWrapper(convertFunc converter.ConvertFunc) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		logrus.WithField("digest", desc.Digest).Infof("converting...")
		return convertFunc(ctx, cs, desc)
	}
}
