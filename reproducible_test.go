package erofs

import (
	"bytes"
	"testing"
	"testing/fstest"
	"time"
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
