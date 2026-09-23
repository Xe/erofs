package erofs

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOnlyBuffer is an io.WriterAt that is not an io.ReaderAt.
type writeOnlyBuffer struct{ b []byte }

func (m *writeOnlyBuffer) WriteAt(p []byte, off int64) (int, error) {
	if need := int(off) + len(p); need > len(m.b) {
		m.b = append(m.b, make([]byte, need-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

func newImageFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func regressDirInfo() fs.FileInfo {
	return &staticFileInfo{name: "d", mode: fs.ModeDir | 0o755, mod: lazyEpoch}
}

// openAndReadAll opens the image, validates it, and checks every file in want.
func openAndReadAll(t *testing.T, img []byte, want map[string][]byte) {
	t.Helper()
	result, err := Validate(bytes.NewReader(img), int64(len(img)))
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Validate: %v, errors: %v", err, result.Errors)
	}
	fsys, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for p, data := range want {
		got, err := fs.ReadFile(fsys, strings.TrimPrefix(p, "/"))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", p, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("ReadFile %s: content differs", p)
		}
	}
}

// An image smaller than one block must build on an *os.File, and the file
// must be padded to the block count in the superblock.
func TestBuildSmallImageOnFile(t *testing.T) {
	f := newImageFile(t)
	b := NewBuilder(f, WithBlockSize(12), WithEpoch(lazyEpoch))
	if err := b.AddFile("/a", lazyInfo("a", 2), []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	img, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(img)%4096 != 0 {
		t.Fatalf("image size %d is not a multiple of the block size", len(img))
	}
	openAndReadAll(t, img, map[string][]byte{"/a": []byte("hi")})
}

// A writer that is not an io.ReaderAt must get the same image as one that is.
func TestBuildWriteOnlyWriter(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []BuildOption
	}{
		{"none", nil},
		{"lz4", []BuildOption{WithCompression(CompressionAutoLZ4)}},
		{"zstd", []BuildOption{WithCompression(CompressionZstd)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string][]byte{
				"/f":     bytes.Repeat([]byte("x"), 5000),
				"/small": []byte("hi"),
			}
			build := func(w interface {
				WriteAt([]byte, int64) (int, error)
			}) {
				opts := append([]BuildOption{WithBlockSize(12), WithEpoch(lazyEpoch)}, tt.opts...)
				b := NewBuilder(w, opts...)
				for _, p := range []string{"/f", "/small"} {
					if err := b.AddFile(p, lazyInfo(p, len(files[p])), files[p]); err != nil {
						t.Fatal(err)
					}
				}
				if err := b.Build(); err != nil {
					t.Fatal(err)
				}
			}
			wo := &writeOnlyBuffer{}
			build(wo)
			rw := newWriterAtBuffer(0)
			build(rw)
			if !bytes.Equal(wo.b, rw.Bytes()) {
				t.Fatal("write-only image differs from read-write image")
			}
			openAndReadAll(t, wo.b, files)
		})
	}
}

// Files added without any AddDir call must get an implicit root and
// implicit parent directories.
func TestBuildImplicitDirs(t *testing.T) {
	for _, tt := range []struct {
		name  string
		paths []string
	}{
		{"root only", []string{"/file-0.txt", "/file-1.txt", "/file-2.txt"}},
		{"one subdir", []string{"/sub/a.txt"}},
		{"deep", []string{"/a/b/c/d.txt", "/a/b/e.txt", "/a/f.txt", "/g.txt"}},
		{"many", func() []string {
			var ps []string
			for i := range 150 {
				ps = append(ps, fmt.Sprintf("/d%d/file-%d.txt", i%7, i))
			}
			return ps
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := make(map[string][]byte)
			buf := newWriterAtBuffer(0)
			b := NewBuilder(buf, WithBlockSize(12), WithEpoch(lazyEpoch))
			for _, p := range tt.paths {
				data := []byte("content of " + p)
				want[p] = data
				if err := b.AddFile(p, lazyInfo(p, len(data)), data); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Build(); err != nil {
				t.Fatal(err)
			}
			openAndReadAll(t, buf.Bytes(), want)
		})
	}
}

// AddDir after a child of that directory was added must keep the child.
func TestBuildAddDirAfterChild(t *testing.T) {
	buf := newWriterAtBuffer(0)
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(lazyEpoch))
	if err := b.AddFile("/sub/a.txt", lazyInfo("a.txt", 2), []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddDir("/sub", regressDirInfo()); err != nil {
		t.Fatal(err)
	}
	if err := b.AddDir("/", regressDirInfo()); err != nil {
		t.Fatal(err)
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	openAndReadAll(t, buf.Bytes(), map[string][]byte{"/sub/a.txt": []byte("hi")})
}
