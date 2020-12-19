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

package zstdchunked

import (
	"context"
	"testing"

	"runtime/debug"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/nativeconverter"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/containerd/stargz-snapshotter/zstdchunked"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestLayerConvertFunc tests zstd:chunked conversion.
// TestLayerConvertFunc is a pure unit test that does not need the daemon to be running.
func TestLayerConvertFunc(t *testing.T) {
	ctx := context.Background()
	desc, cs, err := testutil.EnsureHello(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lcf := LayerConvertFunc(estargz.WithPrioritizedFiles([]string{"hello"}))
	docker2oci := true
	platformMC := platforms.DefaultStrict()
	cf := nativeconverter.IndexConvertFunc(lcf, docker2oci, platformMC)

	newDesc, err := cf(ctx, cs, *desc)
	if err != nil {
		t.Log(string(debug.Stack()))
		t.Fatal(err)
	}

	metadata := make(map[string]string)
	handler := func(hCtx context.Context, hDesc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		for k, v := range hDesc.Annotations {
			if k == estargz.TOCJSONDigestAnnotation ||
				k == zstdchunked.ZstdChunkedManifestChecksumAnnotation ||
				k == zstdchunked.ZstdChunkedManifestPositionAnnotation {
				metadata[k] = v
			}
		}
		return nil, nil
	}
	handlers := images.Handlers(
		images.ChildrenHandler(cs),
		images.HandlerFunc(handler),
	)
	if err := images.Walk(ctx, handlers, *newDesc); err != nil {
		t.Fatal(err)
	}

	if _, ok := metadata[estargz.TOCJSONDigestAnnotation]; !ok {
		t.Errorf("%q is not set", estargz.TOCJSONDigestAnnotation)
	}
	if _, ok := metadata[zstdchunked.ZstdChunkedManifestChecksumAnnotation]; !ok {
		t.Errorf("%q is not set", zstdchunked.ZstdChunkedManifestChecksumAnnotation)
	}
	if _, ok := metadata[zstdchunked.ZstdChunkedManifestPositionAnnotation]; !ok {
		t.Errorf("%q is not set", zstdchunked.ZstdChunkedManifestPositionAnnotation)
	}
}
