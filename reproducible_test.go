package erofs

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// buildWithEpoch builds an image from fsys pinned to epoch and returns the bytes.
func buildWithEpoch(t *testing.T, fsys fstest.MapFS, epoch time.Time) []byte {
	t.Helper()
	buf := newWriterAtBuffer(64 * 1024)
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))
	if err := b.AddFromFS(fsys); err != nil {
		t.Fatalf("AddFromFS: %v", err)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return buf.Bytes()
}

// TestReproducibleIgnoresFileMtime proves that with WithEpoch set, the staged
// files' own modification times do not leak into the image: two builds whose
// only difference is per-file mtime must be byte-identical.
func TestReproducibleIgnoresFileMtime(t *testing.T) {
	epoch := time.Unix(1_700_000_000, 0)

	mk := func(mt time.Time) fstest.MapFS {
		return fstest.MapFS{
			"usr/bin/hello": &fstest.MapFile{Data: []byte("hi"), Mode: 0o755, ModTime: mt},
			"README.md":     &fstest.MapFile{Data: []byte("# hello\n"), Mode: 0o644, ModTime: mt},
		}
	}

	a := buildWithEpoch(t, mk(time.Unix(1_800_000_000, 111_222_333)), epoch)
	b := buildWithEpoch(t, mk(time.Unix(1_900_000_555, 999_888_777)), epoch)

	if !bytes.Equal(a, b) {
		t.Error("image bytes depend on staged file mtime; build is not reproducible")
	}
}

// TestReproducibleBuildTime proves the superblock build time is pinned to the
// epoch (on-disk BuildTime == 0) rather than stamped from the wall clock.
func TestReproducibleBuildTime(t *testing.T) {
	epoch := time.Unix(1_700_000_000, 0)
	buf := newWriterAtBuffer(64 * 1024)
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))
	if err := b.AddFromFS(fstest.MapFS{
		"README.md": &fstest.MapFile{Data: []byte("# hello\n"), Mode: 0o644, ModTime: epoch},
	}); err != nil {
		t.Fatalf("AddFromFS: %v", err)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var sb ondisk.SuperBlock
	sr := io.NewSectionReader(bytes.NewReader(buf.Bytes()), ondisk.SuperOffset, ondisk.SuperBlockSize)
	if err := binary.Read(sr, binary.LittleEndian, &sb); err != nil {
		t.Fatalf("decode superblock: %v", err)
	}

	if sb.BuildTime != 0 {
		t.Errorf("superblock BuildTime = %d, want 0 (pinned to epoch)", sb.BuildTime)
	}
	if sb.Epoch != epoch.Unix() {
		t.Errorf("superblock Epoch = %d, want %d", sb.Epoch, epoch.Unix())
	}
}
