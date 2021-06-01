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

package snapshot

import (
	"context"
	_ "crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/containerd/snapshots/testsuite"
)

const (
	remoteSampleFile         = "foo"
	remoteSampleFileContents = "remote layer"
	brokenLabel              = "containerd.io/snapshot/broken"
)

func prepareWithTarget(t *testing.T, sn snapshots.Snapshotter, target, key, parent string, labels map[string]string) string {
	ctx := context.TODO()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[targetSnapshotLabel] = target
	if _, err := sn.Prepare(ctx, key, parent, snapshots.WithLabels(labels)); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("failed to prepare remote snapshot: %v", err)
	}
	return target
}

func TestRemotePrepare(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(context.TODO(), root, bindFileSystem(t), CleanupCommitted, RestoreSnapshots)
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareWithTarget(t, sn, "testTarget", "/tmp/prepareTarget", "", nil)
	defer sn.Remove(ctx, target)

	// Get internally committed remote snapshot.
	var tinfo *snapshots.Info
	if err := sn.Walk(ctx, func(ctx context.Context, i snapshots.Info) error {
		if tinfo == nil && i.Kind == snapshots.KindCommitted {
			if i.Labels[targetSnapshotLabel] != target {
				return nil
			}
			if i.Parent != "" {
				return nil
			}
			tinfo = &i
		}
		return nil

	}); err != nil {
		t.Fatalf("failed to get remote snapshot: %v", err)
	}
	if tinfo == nil {
		t.Fatalf("prepared remote snapshot %q not found", target)
	}

	// Stat and validate the remote snapshot.
	info, err := sn.Stat(ctx, tinfo.Name)
	if err != nil {
		t.Fatal("failed to stat remote snapshot")
	}
	if info.Kind != snapshots.KindCommitted {
		t.Errorf("snapshot Kind is %q; want %q", info.Kind, snapshots.KindCommitted)
	}
	if label, ok := info.Labels[targetSnapshotLabel]; !ok || label != target {
		t.Errorf("remote snapshot hasn't valid remote label: %q", label)
	}
}

func TestRemoteOverlay(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(context.TODO(), root, bindFileSystem(t), CleanupCommitted, RestoreSnapshots)
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareWithTarget(t, sn, "testTarget", "/tmp/prepareTarget", "", nil)
	defer sn.Remove(ctx, target)

	// Prepare a new layer based on the remote snapshot.
	pKey := "/tmp/test"
	mounts, err := sn.Prepare(ctx, pKey, target)
	if err != nil {
		t.Fatalf("faild to prepare using lower remote layer: %v", err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("expected source %q but received %q", "overlay", m.Source)
	}
	var (
		bp    = getBasePath(ctx, sn, root, pKey)
		work  = "workdir=" + filepath.Join(bp, "work")
		upper = "upperdir=" + filepath.Join(bp, "fs")
		lower = "lowerdir=" + getParents(ctx, sn, root, pKey)[0]
	)
	for i, v := range []string{
		work,
		upper,
		lower,
	} {
		if m.Options[i] != v {
			t.Errorf("expected %q but received %q", v, m.Options[i])
		}
	}

	// Validate the contents of the snapshot
	data, err := ioutil.ReadFile(filepath.Join(getParents(ctx, sn, root, pKey)[0], remoteSampleFile))
	if err != nil {
		t.Fatalf("failed to read a file in the remote snapshot: %v", err)
	}
	if e := string(data); e != remoteSampleFileContents {
		t.Fatalf("expected file contents %q but got %q", remoteSampleFileContents, e)
	}
}

func TestRemoteCommit(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(context.TODO(), root, bindFileSystem(t), CleanupCommitted, RestoreSnapshots)
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareWithTarget(t, sn, "testTarget", "/tmp/prepareTarget", "", nil)
	defer sn.Remove(ctx, target)

	// Prepare a new snapshot based on the remote snapshot
	pKey := "/tmp/test"
	mounts, err := sn.Prepare(ctx, pKey, target)
	if err != nil {
		t.Fatal(err)
	}

	// Make a new active snapshot based on the remote snapshot.
	snapshot, err := ioutil.TempDir("", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(snapshot)
	m := mounts[0]
	if err := m.Mount(snapshot); err != nil {
		t.Fatal(err)
	}
	defer mount.Unmount(snapshot, 0)
	if err := ioutil.WriteFile(filepath.Join(snapshot, "bar"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	mount.Unmount(snapshot, 0)

	// Commit the active snapshot
	cKey := "/tmp/layer"
	if err := sn.Commit(ctx, cKey, pKey); err != nil {
		t.Fatal(err)
	}

	// Validate the committed snapshot
	check, err := ioutil.TempDir("", "check")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(check)
	mounts, err = sn.Prepare(ctx, "/tmp/test2", cKey)
	if err != nil {
		t.Fatal(err)
	}
	m = mounts[0]
	if err := m.Mount(check); err != nil {
		t.Fatal(err)
	}
	defer mount.Unmount(check, 0)
	data, err := ioutil.ReadFile(filepath.Join(check, "bar"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(data); e != "hi" {
		t.Fatalf("expected file contents %q but got %q", "hi", e)
	}
}

func TestFailureDetection(t *testing.T) {
	testutil.RequiresRoot(t)
	tests := []struct {
		name    string
		broken  []bool // top element is the lowest layer
		overlay bool   // whether appending the topmost normal overlay snapshot or not
		wantOK  bool
	}{
		{
			name:   "flat_ok",
			broken: []bool{false},
			wantOK: true,
		},
		{
			name:   "deep_ok",
			broken: []bool{false, false, false},
			wantOK: true,
		},
		{
			name:    "flat_overlay_ok",
			broken:  []bool{false},
			overlay: true,
			wantOK:  true,
		},
		{
			name:    "deep_overlay_ok",
			broken:  []bool{false, false, false},
			overlay: true,
			wantOK:  true,
		},
		{
			name:   "flat_ng",
			broken: []bool{true},
			wantOK: false,
		},
		{
			name:   "deep_ng",
			broken: []bool{false, true, false},
			wantOK: false,
		},
		{
			name:    "flat_overlay_ng",
			broken:  []bool{true},
			overlay: true,
			wantOK:  false,
		},
		{
			name:    "deep_overlay_ng",
			broken:  []bool{false, true, false},
			overlay: true,
			wantOK:  false,
		},
	}

	check := func(t *testing.T, ok bool, err error) bool {
		if err == nil {
			if !ok {
				t.Error("check all passed but wanted to be failed")
				return false
			}
		} else if errdefs.IsUnavailable(err) {
			if ok {
				t.Error("got Unavailable but wanted to be non-error")
				return false
			}
		} else {
			t.Errorf("got unexpected error %q", err)
			return false
		}
		return true
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.TODO()
			root, err := ioutil.TempDir("", "remote")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			fi := bindFileSystem(t)
			sn, err := NewSnapshotter(context.TODO(), root, fi, CleanupCommitted, RestoreSnapshots)
			if err != nil {
				t.Fatalf("failed to make new Snapshotter: %q", err)
			}
			fs, ok := fi.(*bindFs)
			if !ok {
				t.Fatalf("Invalid filesystem type(not *filesystem)")
			}

			// Prepare snapshots
			fs.checkFailure = false
			pKey := ""
			for i, broken := range tt.broken {
				var (
					targetName = fmt.Sprintf("/tmp/testTarget%d", i)
					key        = fmt.Sprintf("/tmp/testKey%d", i)
					labels     = make(map[string]string)
				)
				if broken {
					labels[brokenLabel] = "true"
				}
				target := prepareWithTarget(t, sn, targetName, key, pKey, labels)
				defer sn.Remove(ctx, target)
				pKey = target
			}

			if tt.overlay {
				key := "/tmp/test"
				_, err := sn.Prepare(ctx, key, pKey)
				if err != nil {
					t.Fatal(err)
				}
				cKey := "/tmp/layer"
				if err := sn.Commit(ctx, cKey, key); err != nil {
					t.Fatal(err)
				}
				defer sn.Remove(ctx, cKey)
				pKey = cKey
			}

			// Tests if we can detect layer unavailablity
			key := "/tmp/snapshot.test"
			fs.checkFailure = true
			defer sn.Remove(ctx, key)
			if _, err := sn.Prepare(ctx, key, pKey); !check(t, tt.wantOK, err) {
				return
			}
			fs.checkFailure = false
			key2 := "/tmp/test2"
			if _, err = sn.Prepare(ctx, key2, pKey); err != nil {
				t.Fatal(err)
			}
			defer sn.Remove(ctx, key2)
			fs.checkFailure = true
			if _, err := sn.Mounts(ctx, key2); !check(t, tt.wantOK, err) {
				return
			}
		})
	}
}

func bindFileSystem(t *testing.T) FileSystem {
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatalf("failed to prepare working-space for bind filesystem: %q", err)
	}
	if err := ioutil.WriteFile(filepath.Join(root, remoteSampleFile), []byte(remoteSampleFileContents), 0660); err != nil {
		t.Fatalf("failed to write sample file of bind filesystem: %q", err)
	}
	return &bindFs{
		root:   root,
		t:      t,
		broken: make(map[string]bool),
	}
}

type bindFs struct {
	t            *testing.T
	root         string
	checkFailure bool
	broken       map[string]bool
}

func (fs *bindFs) Mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	if _, ok := labels[brokenLabel]; ok {
		fs.broken[mountpoint] = true
	}
	if err := syscall.Mount(fs.root, mountpoint, "none", syscall.MS_BIND, ""); err != nil {
		fs.t.Fatalf("failed to bind mount %q to %q: %v", fs.root, mountpoint, err)
	}
	return nil
}

func (fs *bindFs) Check(ctx context.Context, mountpoint string, labels map[string]string) error {
	if fs.checkFailure {
		if broken, ok := fs.broken[mountpoint]; ok && broken {
			return fmt.Errorf("broken")
		}
	}
	return nil
}

func (fs *bindFs) Unmount(ctx context.Context, mountpoint string) error {
	return syscall.Unmount(mountpoint, 0)
}

func dummyFileSystem() FileSystem { return &dummyFs{} }

type dummyFs struct{}

func (fs *dummyFs) Mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	return fmt.Errorf("dummy")
}

func (fs *dummyFs) Check(ctx context.Context, mountpoint string, labels map[string]string) error {
	return fmt.Errorf("dummy")
}

func (fs *dummyFs) Unmount(ctx context.Context, mountpoint string) error {
	return fmt.Errorf("dummy")
}

// =============================================================================
// Tests backword-comaptibility of overlayfs snapshotter.

func newSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
	snapshotter, err := NewSnapshotter(context.TODO(), root, dummyFileSystem(),
		CleanupCommitted, RestoreSnapshots)
	if err != nil {
		return nil, nil, err
	}

	return snapshotter, func() error { return snapshotter.Close() }, nil
}

func TestOverlay(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, "Overlay", newSnapshotter)
}

func TestOverlayMounts(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := o.Prepare(ctx, "/tmp/test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := filepath.Join(root, "snapshots", "1", "fs")
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}
	if m.Options[0] != "rw" {
		t.Errorf("expected mount option rw but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}
}

func TestOverlayCommit(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayOverlayMount(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	if _, err = o.Prepare(ctx, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	var mounts []mount.Mount
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("expected source %q but received %q", "overlay", m.Source)
	}
	var (
		bp    = getBasePath(ctx, o, root, "/tmp/layer2")
		work  = "workdir=" + filepath.Join(bp, "work")
		upper = "upperdir=" + filepath.Join(bp, "fs")
		lower = "lowerdir=" + getParents(ctx, o, root, "/tmp/layer2")[0]
	)
	for i, v := range []string{
		work,
		upper,
		lower,
	} {
		if m.Options[i] != v {
			t.Errorf("expected %q but received %q", v, m.Options[i])
		}
	}
}

func getBasePath(ctx context.Context, sn snapshots.Snapshotter, root, key string) string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()

	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}

	return filepath.Join(root, "snapshots", s.ID)
}

func getParents(ctx context.Context, sn snapshots.Snapshotter, root, key string) []string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()
	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}
	parents := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parents[i] = filepath.Join(root, "snapshots", s.ParentIDs[i], "fs")
	}
	return parents
}

func TestOverlayOverlayRead(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := mount.All(mounts, dest); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(dest, 0)
	data, err := ioutil.ReadFile(filepath.Join(dest, "foo"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(data); e != "hi" {
		t.Fatalf("expected file contents hi but got %q", e)
	}
}

func TestOverlayView(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/base"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}

	key = "/tmp/top"
	_, err = o.Prepare(ctx, key, "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(getParents(ctx, o, root, "/tmp/top")[0], "foo"), []byte("hi, again"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "top", key); err != nil {
		t.Fatal(err)
	}

	mounts, err = o.View(ctx, "/tmp/view1", "base")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := getParents(ctx, o, root, "/tmp/view1")[0]
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}
	if m.Options[0] != "ro" {
		t.Errorf("expected mount option ro but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}

	mounts, err = o.View(ctx, "/tmp/view2", "top")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("mount source should be overlay but received %q", m.Source)
	}
	if len(m.Options) != 1 {
		t.Errorf("expected 1 mount option but got %d", len(m.Options))
	}
	lowers := getParents(ctx, o, root, "/tmp/view2")
	expected = fmt.Sprintf("lowerdir=%s:%s", lowers[0], lowers[1])
	if m.Options[0] != expected {
		t.Errorf("expected option %q but received %q", expected, m.Options[0])
	}
}
