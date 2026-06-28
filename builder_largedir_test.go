package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestBuilderLargeDirectory exercises a directory whose entries do not fit in a
// single block, so the directory inode stores out-of-line data blocks plus an
// inline tail. This previously produced corrupt images for two independent
// reasons:
//
//  1. The superblock BlocksLo undercounted (it only summed FLAT_PLAIN extents),
//     so the directory's own out-of-line data block fell past the declared end
//     of the filesystem.
//  2. The extended inode wrote nlink into the i_nb field (offset 6), which is
//     startblk_hi/blocks_hi -- the high bits of the block address -- so the
//     directory's out-of-line block address was corrupted (fsck reported
//     "invalid de[0].nameoff 0 @ ... lblk 0").
//
// The test verifies the image round-trips through this reader, that every entry
// is listed, that BlocksLo covers the whole image, and -- when fsck.erofs is
// available -- that the reference tool accepts the image.
func TestBuilderLargeDirectory(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// 300 entries of ~20 bytes each (12-byte dirent + 8-byte name) spill the
	// root directory well past one 4 KiB block.
	const nfiles = 300
	want := make(map[string][]byte, nfiles)

	buf := newWriterAtBuffer(8 << 20)
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	for i := range nfiles {
		name := fmt.Sprintf("f%04d.txt", i)
		data := fmt.Appendf(nil, "contents of file %d\n", i)
		want["/"+name] = data
		b.AddFile("/"+name, &staticFileInfo{
			name: name, mode: 0o644, size: int64(len(data)), mod: epoch,
		}, data)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	img := buf.Bytes()

	// The root directory must actually need out-of-line blocks for this test to
	// exercise the bug; otherwise it would fit inline and never hit it.
	var root *buildInode
	for _, ino := range b.inodes {
		if ino.path == "/" {
			root = ino
		}
	}
	if root == nil {
		t.Fatal("root inode not found")
	}
	if root.size <= int64(b.blockSize) {
		t.Fatalf("root dir size %d <= block size %d; test does not exercise out-of-line dir blocks", root.size, b.blockSize)
	}

	fsys, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Every entry must be listed exactly once.
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != nfiles {
		t.Fatalf("ReadDir returned %d entries, want %d", len(ents), nfiles)
	}
	gotNames := make([]string, len(ents))
	for i, e := range ents {
		gotNames[i] = "/" + e.Name()
	}
	sort.Strings(gotNames)
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	sort.Strings(wantNames)
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("entry %d = %q, want %q", i, gotNames[i], wantNames[i])
		}
	}

	// Every file must read back byte-for-byte.
	for name, data := range want {
		got, err := fs.ReadFile(fsys, name[1:])
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%s: content mismatch", name)
		}
	}

	// BlocksLo (__le32 at superblock offset 36) must cover the directory's
	// out-of-line data block. The original bug reported BlocksLo == startBlk,
	// leaving the directory's own block one past the declared end of the
	// filesystem.
	blocksLo := binary.LittleEndian.Uint32(img[1024+36:])
	if int64(blocksLo) <= int64(root.startBlk) {
		t.Fatalf("BlocksLo = %d does not include the root dir data block at %d", blocksLo, root.startBlk)
	}

	// Strongest check: the reference tool must accept the image.
	if fsck, err := exec.LookPath("fsck.erofs"); err == nil {
		path := filepath.Join(t.TempDir(), "largedir.img")
		if err := os.WriteFile(path, img, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(fsck, path).CombinedOutput(); err != nil {
			t.Fatalf("fsck.erofs rejected the image: %v\n%s", err, out)
		}
	} else {
		t.Log("fsck.erofs not installed; skipped reference validation")
	}
}
