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

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
)

func TestSort(t *testing.T) {
	longname1 := longstring(120)
	longname2 := longstring(150)

	tests := []struct {
		name             string
		in               []tarEntry
		log              []string // MUST NOT include "./" prefix here
		want             []tarEntry
		wantFail         bool
		allowMissedFiles []string
	}{
		{
			name: "nolog",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
			),
		},
		{
			name: "identical",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
			log: []string{"foo.txt", "bar/baz.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				prefetchLandmark(),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
		},
		{
			name: "shuffle_reg",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "bar.txt", "baz.txt"},
			want: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "shuffle_directory",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				dir("baz/"),
				file("baz/baz1.txt", "baz"),
				file("baz/baz2.txt", "baz"),
				dir("baz/bazbaz/"),
				file("baz/bazbaz/bazbaz_b.txt", "baz"),
				file("baz/bazbaz/bazbaz_a.txt", "baz"),
			),
			log: []string{"baz/bazbaz/bazbaz_a.txt", "baz/baz2.txt", "foo.txt"},
			want: tarOf(
				dir("baz/"),
				dir("baz/bazbaz/"),
				file("baz/bazbaz/bazbaz_a.txt", "baz"),
				file("baz/baz2.txt", "baz"),
				file("foo.txt", "foo"),
				prefetchLandmark(),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				file("baz/baz1.txt", "baz"),
				file("baz/bazbaz/bazbaz_b.txt", "baz"),
			),
		},
		{
			name: "shuffle_link",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				link("bar.txt", "baz.txt"),
				file("baa.txt", "baa"),
			),
			log: []string{"baz.txt"},
			want: tarOf(
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				link("bar.txt", "baz.txt"),
				file("baa.txt", "baa"),
			),
		},
		{
			name: "longname",
			in: tarOf(
				file("foo.txt", "foo"),
				file(longname1, "test"),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				file(fmt.Sprintf("bar/%s", longname2), "test2"),
			),
			log: []string{fmt.Sprintf("bar/%s", longname2), longname1},
			want: tarOf(
				dir("bar/"),
				file(fmt.Sprintf("bar/%s", longname2), "test2"),
				file(longname1, "test"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
		},
		{
			name: "various_types",
			in: tarOf(
				file("foo.txt", "foo"),
				symlink("foo2", "foo.txt"),
				chardev("foochar", 10, 50),
				blockdev("fooblock", 15, 20),
				fifo("fifoo"),
			),
			log: []string{"fifoo", "foo2", "foo.txt", "fooblock"},
			want: tarOf(
				fifo("fifoo"),
				symlink("foo2", "foo.txt"),
				file("foo.txt", "foo"),
				blockdev("fooblock", 15, 20),
				prefetchLandmark(),
				chardev("foochar", 10, 50),
			),
		},
		{
			name: "existing_landmark",
			in: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("baa.txt", "baa"),
				file("baz.txt", "baz"),
			),
		},
		{
			name: "existing_landmark_nolog",
			in: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "existing_noprefetch_landmark",
			in: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("baa.txt", "baa"),
				file("baz.txt", "baz"),
			),
		},
		{
			name: "existing_noprefetch_landmark_nolog",
			in: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "not_existing_file",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log:      []string{"baa.txt", "bar.txt", "dummy"},
			want:     tarOf(),
			wantFail: true,
		},
		{
			name: "not_existing_file_allow_fail",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "dummy1", "bar.txt", "dummy2"},
			want: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
			),
			allowMissedFiles: []string{"dummy1", "dummy2"},
		},
		{
			name: "duplicated_entry",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				dir("bar/"),
				file("bar/baz.txt", "baz2"),
			),
			log: []string{"bar/baz.txt"},
			want: tarOf(
				dir("bar/"),
				file("bar/baz.txt", "baz2"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "hardlink",
			in: tarOf(
				file("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
			),
			log: []string{"bazlink"},
			want: tarOf(
				file("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
				prefetchLandmark(),
			),
		},
		{
			name: "root_relative_file",
			in: tarOf(
				dir("./"),
				file("./foo.txt", "foo"),
				file("./baz.txt", "baz"),
				file("./baa.txt", "baa"),
				link("./bazlink", "./baz.txt"),
			),
			log: []string{"baa.txt", "bazlink"},
			want: tarOf(
				dir("./"),
				file("./baa.txt", "baa"),
				file("./baz.txt", "baz"),
				link("./bazlink", "./baz.txt"),
				prefetchLandmark(),
				file("./foo.txt", "foo"),
			),
		},
		{
			// GNU tar accepts tar containing absolute path
			name: "root_absolute_file",
			in: tarOf(
				dir("/"),
				file("/foo.txt", "foo"),
				file("/baz.txt", "baz"),
				file("/baa.txt", "baa"),
				link("/bazlink", "/baz.txt"),
			),
			log: []string{"baa.txt", "bazlink"},
			want: tarOf(
				dir("/"),
				file("/baa.txt", "baa"),
				file("/baz.txt", "baz"),
				link("/bazlink", "/baz.txt"),
				prefetchLandmark(),
				file("/foo.txt", "foo"),
			),
		},
	}
	for _, tt := range tests {
		for _, logprefix := range allowedPrefix {
			logprefix := logprefix
			for _, tarprefix := range allowedPrefix {
				tarprefix := tarprefix
				t.Run(fmt.Sprintf("%s-logprefix=%q-tarprefix=%q", tt.name, logprefix, tarprefix), func(t *testing.T) {
					// Sort tar file
					var pfiles []string
					for _, f := range tt.log {
						pfiles = append(pfiles, logprefix+f)
					}
					var opts []Option
					var missedFiles []string
					if tt.allowMissedFiles != nil {
						opts = append(opts, WithAllowPrioritizeNotFound(&missedFiles))
					}
					rc, err := Build(buildTarStatic(t, tt.in, tarprefix),
						append(opts, WithPrioritizedFiles(pfiles))...)
					if tt.wantFail {
						if err != nil {
							return
						}
						t.Errorf("This test must fail but succeeded")
						return
					}
					if err != nil {
						t.Errorf("failed to build stargz: %v", err)
					}
					zr, err := gzip.NewReader(rc)
					if err != nil {
						t.Fatalf("failed to create gzip reader: %v", err)
					}
					defer zr.Close()
					if tt.allowMissedFiles != nil {
						want := map[string]struct{}{}
						for _, f := range tt.allowMissedFiles {
							want[logprefix+f] = struct{}{}
						}
						got := map[string]struct{}{}
						for _, f := range missedFiles {
							got[f] = struct{}{}
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("unexpected missed files: want %v, got: %v",
								want, got)
							return
						}
					}
					gotTar := tar.NewReader(zr)

					// Compare all
					wantTar := tar.NewReader(buildTarStatic(t, tt.want, tarprefix))
					for {
						// Fetch and parse next header.
						gotH, wantH, err := next(t, gotTar, wantTar)
						if err != nil {
							if err == io.EOF {
								break
							} else {
								t.Fatalf("Failed to parse tar file: %v", err)
							}
						}

						if !reflect.DeepEqual(gotH, wantH) {
							t.Errorf("different header (got = name:%q,type:%d,size:%d; want = name:%q,type:%d,size:%d)",
								gotH.Name, gotH.Typeflag, gotH.Size, wantH.Name, wantH.Typeflag, wantH.Size)
							return

						}

						got, err := ioutil.ReadAll(gotTar)
						if err != nil {
							t.Fatal("failed to read got tar payload")
						}
						want, err := ioutil.ReadAll(wantTar)
						if err != nil {
							t.Fatal("failed to read want tar payload")
						}
						if !bytes.Equal(got, want) {
							t.Errorf("different payload (got = %q; want = %q)", string(got), string(want))
							return
						}
					}
				})
			}
		}
	}
}

func next(t *testing.T, a *tar.Reader, b *tar.Reader) (ah *tar.Header, bh *tar.Header, err error) {
	eofA, eofB := false, false

	ah, err = nextWithSkipTOC(a)
	if err != nil {
		if err == io.EOF {
			eofA = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	bh, err = nextWithSkipTOC(b)
	if err != nil {
		if err == io.EOF {
			eofB = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	if eofA != eofB {
		if !eofA {
			t.Logf("a = %q", ah.Name)
		}
		if !eofB {
			t.Logf("b = %q", bh.Name)
		}
		t.Fatalf("got eof %t != %t", eofB, eofA)
	}
	if eofA {
		err = io.EOF
	}

	return
}

func nextWithSkipTOC(a *tar.Reader) (h *tar.Header, err error) {
	if h, err = a.Next(); err != nil {
		return
	}
	if h.Name == TOCTarName {
		return nextWithSkipTOC(a)
	}
	return
}

func longstring(size int) (str string) {
	unit := "long"
	for i := 0; i < size/len(unit)+1; i++ {
		str = fmt.Sprintf("%s%s", str, unit)
	}

	return str[:size]
}

func TestCountReader(t *testing.T) {
	tests := []struct {
		name    string
		ops     func(*countReader) error
		wantPos int64
	}{
		{
			name: "nop",
			ops: func(pw *countReader) error {
				return nil
			},
			wantPos: 0,
		},
		{
			name: "read",
			ops: func(pw *countReader) error {
				size := 5
				if _, err := pw.Read(make([]byte, size)); err != nil {
					return err
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "readtwice",
			ops: func(pw *countReader) error {
				size1, size2 := 5, 3
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size2)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_start",
			ops: func(pw *countReader) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_start_twice",
			ops: func(pw *countReader) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 3,
		},
		{
			name: "seek_current",
			ops: func(pw *countReader) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_current_twice",
			ops: func(pw *countReader) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_current_twice_negative",
			ops: func(pw *countReader) error {
				size1, size2 := int64(5), int64(-3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 2,
		},
		{
			name: "mixed",
			ops: func(pw *countReader) error {
				size1, size2, size3, size4, size5 := int64(5), int64(-3), int64(4), int64(-1), int64(6)
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size3, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size4, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size5)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw, err := newCountReader(bytes.NewReader(make([]byte, 100)))
			if err != nil {
				t.Fatalf("failed to make position watcher: %q", err)
			}

			if err := tt.ops(pw); err != nil {
				t.Fatalf("failed to operate position watcher: %q", err)
			}

			gotPos := pw.currentPos()
			if tt.wantPos != gotPos {
				t.Errorf("current position: %d; want %d", gotPos, tt.wantPos)
			}
		})
	}

}
