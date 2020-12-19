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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"testing"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/klauspost/compress/zstd"
)

// TestZstdChunked tests zstd:chunked
func TestZstdChunked(t *testing.T) {
	estargz.CompressionTestSuite(t,
		zstdControllerWithLevel(zstd.SpeedFastest),
		zstdControllerWithLevel(zstd.SpeedDefault),
		zstdControllerWithLevel(zstd.SpeedBetterCompression),
		zstdControllerWithLevel(zstd.SpeedBestCompression),
	)
}

func zstdControllerWithLevel(compressionLevel zstd.EncoderLevel) estargz.TestingController {
	return &zstdController{&Compressor{CompressionLevel: compressionLevel}, &Decompressor{}}
}

type zstdController struct {
	*Compressor
	*Decompressor
}

func (zc *zstdController) String() string {
	return fmt.Sprintf("zstd_compression_level=%v", zc.Compressor.CompressionLevel)
}

func (zc *zstdController) CountStreams(t *testing.T, b []byte) (numStreams int) {
	t.Logf("got zstd streams (compressed size: %d):", len(b))
	zh := new(zstd.Header)
	magicLen := 4 // length of magic bytes and skippable frame magic bytes
	zoff := 0
	for {
		if len(b) <= zoff {
			break
		} else if len(b)-zoff <= magicLen {
			t.Fatalf("invalid frame size %d is too small", len(b)-zoff)
		}
		remainingFrames := b[zoff:]

		// Check if zoff points to the beginning of a frame
		if !bytes.Equal(remainingFrames[:magicLen], zstdFrameMagic) {
			if !bytes.Equal(remainingFrames[:magicLen], skippableFrameMagic) {
				t.Fatalf("frame must start from magic bytes; but %x",
					remainingFrames[:magicLen])
			}

			// This is a skippable frame
			size := binary.LittleEndian.Uint32(remainingFrames[magicLen : magicLen+4])
			t.Logf("  [%d] at %d in stargz, SKIPPABLE FRAME (nextFrame: %d/%d)",
				numStreams, zoff, zoff+(magicLen+4+int(size)), len(b))
			zoff += (magicLen + 4 + int(size))
			numStreams++
			continue
		}

		// Parse header and get uncompressed size of this frame
		if err := zh.Decode(remainingFrames); err != nil {
			t.Fatalf("countStreams(zstd), *Header.Decode: %v", err)
		}
		uncompressedFrameSize := zh.FrameContentSize
		if uncompressedFrameSize == 0 {
			// FrameContentSize is optional so it's possible we cannot get size info from
			// this field. If this frame contains only one block, we can get the decompressed
			// size from that block header.
			if zh.FirstBlock.OK && zh.FirstBlock.Last && !zh.FirstBlock.Compressed {
				uncompressedFrameSize = uint64(zh.FirstBlock.DecompressedSize)
			} else {
				t.Fatalf("countStreams(zstd), failed to get uncompressed frame size")
			}
		}

		// Identify the offset of the next frame
		nextFrame := magicLen // ignore the magic bytes of this frame
		for {
			// search for the beginning magic bytes of the next frame
			searchBase := nextFrame
			nextMagicIdx := nextIndex(remainingFrames[searchBase:], zstdFrameMagic)
			nextSkippableIdx := nextIndex(remainingFrames[searchBase:], skippableFrameMagic)
			nextFrame = len(remainingFrames)
			for _, i := range []int{nextMagicIdx, nextSkippableIdx} {
				if 0 < i && searchBase+i < nextFrame {
					nextFrame = searchBase + i
				}
			}

			// "nextFrame" seems the offset of the next frame. Verify it by checking if
			// the decompressed size of this frame is the same value as set in the header.
			zr, err := zstd.NewReader(bytes.NewReader(remainingFrames[:nextFrame]))
			if err != nil {
				t.Logf("  [%d] invalid frame candidate: %v", numStreams, err)
				continue
			}
			defer zr.Close()
			res, err := ioutil.ReadAll(zr)
			if err != nil && err != io.ErrUnexpectedEOF {
				t.Fatalf("countStreams(zstd), ReadAll: %v", err)
			}
			if uint64(len(res)) == uncompressedFrameSize {
				break
			}

			// Try the next magic byte candidate until end
			if uint64(len(res)) > uncompressedFrameSize || nextFrame > len(remainingFrames) {
				t.Fatalf("countStreams(zstd), cannot identify frame (off:%d)", zoff)
			}
		}
		t.Logf("  [%d] at %d in stargz, uncompressed length %d (nextFrame: %d/%d)",
			numStreams, zoff, uncompressedFrameSize, zoff+nextFrame, len(b))
		zoff += nextFrame
		numStreams++
	}
	return numStreams
}

func nextIndex(s1, sub []byte) int {
	for i := 0; i < len(s1); i++ {
		if len(s1)-i < len(sub) {
			return -1
		} else if bytes.Equal(s1[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func (zc *zstdController) DiffIDOf(t *testing.T, b []byte) string {
	h := sha256.New()
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("diffIDOf(zstd): %v", err)
	}
	defer zr.Close()
	if _, err := io.Copy(h, zr); err != nil {
		t.Fatalf("diffIDOf(zstd).Copy: %v", err)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// Tests footer encoding, size, and parsing of zstd:chunked.
func TestZstdChunkedFooter(t *testing.T) {
	max := int64(200000)
	for off := int64(0); off <= max; off += 1023 {
		size := max - off
		checkZstdChunkedFooter(t, off, size, size/2)
	}
}

func checkZstdChunkedFooter(t *testing.T, off, size, cSize int64) {
	footer := zstdFooterBytes(uint64(off), uint64(size), uint64(cSize))
	if len(footer) != FooterSize {
		t.Fatalf("for offset %v, footer length was %d, not expected %d. got bytes: %q", off, len(footer), FooterSize, footer)
	}
	gotOff, gotSize, err := (&Decompressor{}).ParseFooter(footer)
	if err != nil {
		t.Fatalf("failed to parse footer for offset %d, footer: %x: err: %v",
			off, footer, err)
	}
	if gotOff != off {
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = off %d; want %d", off, gotOff, off)
	}
	if gotSize != cSize {
		t.Fatalf("ParseFooter(footerBytes(offset %d)) = size %d; want %d", off, gotSize, cSize)
	}
}
