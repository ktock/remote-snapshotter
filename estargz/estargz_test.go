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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the LICENSE file.
*/

package estargz

import "testing"

// Tests *Reader.ChunkEntryForOffset about offset and size calculation.
func TestChunkEntryForOffset(t *testing.T) {
	const chunkSize = 4
	tests := []struct {
		name            string
		fileSize        int64
		reqOffset       int64
		wantOk          bool
		wantChunkOffset int64
		wantChunkSize   int64
	}{
		{
			name:            "1st_chunk_in_1_chunk_reg",
			fileSize:        chunkSize * 1,
			reqOffset:       chunkSize * 0,
			wantChunkOffset: chunkSize * 0,
			wantChunkSize:   chunkSize,
			wantOk:          true,
		},
		{
			name:      "2nd_chunk_in_1_chunk_reg",
			fileSize:  chunkSize * 1,
			reqOffset: chunkSize * 1,
			wantOk:    false,
		},
		{
			name:            "1st_chunk_in_2_chunks_reg",
			fileSize:        chunkSize * 2,
			reqOffset:       chunkSize * 0,
			wantChunkOffset: chunkSize * 0,
			wantChunkSize:   chunkSize,
			wantOk:          true,
		},
		{
			name:            "2nd_chunk_in_2_chunks_reg",
			fileSize:        chunkSize * 2,
			reqOffset:       chunkSize * 1,
			wantChunkOffset: chunkSize * 1,
			wantChunkSize:   chunkSize,
			wantOk:          true,
		},
		{
			name:      "3rd_chunk_in_2_chunks_reg",
			fileSize:  chunkSize * 2,
			reqOffset: chunkSize * 2,
			wantOk:    false,
		},
	}

	for _, te := range tests {
		t.Run(te.name, func(t *testing.T) {
			name := "test"
			_, r := regularFileReader(name, te.fileSize, chunkSize)
			ce, ok := r.ChunkEntryForOffset(name, te.reqOffset)
			if ok != te.wantOk {
				t.Errorf("ok = %v; want (%v)", ok, te.wantOk)
			} else if ok {
				if !(ce.ChunkOffset == te.wantChunkOffset && ce.ChunkSize == te.wantChunkSize) {
					t.Errorf("chunkOffset = %d, ChunkSize = %d; want (chunkOffset = %d, chunkSize = %d)",
						ce.ChunkOffset, ce.ChunkSize, te.wantChunkOffset, te.wantChunkSize)
				}
			}
		})
	}
}

// regularFileReader makes a minimal Reader of "reg" and "chunk" without tar-related information.
func regularFileReader(name string, size int64, chunkSize int64) (*TOCEntry, *Reader) {
	ent := &TOCEntry{
		Name: name,
		Type: "reg",
	}
	m := ent
	chunks := make([]*TOCEntry, 0, size/chunkSize+1)
	var written int64
	for written < size {
		remain := size - written
		cs := chunkSize
		if remain < cs {
			cs = remain
		}
		ent.ChunkSize = cs
		ent.ChunkOffset = written
		chunks = append(chunks, ent)
		written += cs
		ent = &TOCEntry{
			Name: name,
			Type: "chunk",
		}
	}

	if len(chunks) == 1 {
		chunks = nil
	}
	return m, &Reader{
		m:      map[string]*TOCEntry{name: m},
		chunks: map[string][]*TOCEntry{name: chunks},
	}
}
