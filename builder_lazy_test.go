package erofs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var lazyEpoch = time.Unix(1735689600, 0)

func lazyInfo(name string, size int) fs.FileInfo {
	return &staticFileInfo{name: name, mode: 0o644, size: int64(size), mod: lazyEpoch}
}

func lazyContent(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*37 + i/97)
	}
	return data
}

func TestAddFileFuncMatchesAddFile(t *testing.T) {
	files := []struct {
		name string
		size int
	}{
		{"empty", 0}, {"tiny", 1}, {"max inline", 4096 - 64},
		{"over inline", 4096 - 63}, {"one block", 4096},
		{"block plus one", 4097}, {"multi block", 3*4096 + 17},
		{"large", 10<<20 + 5},
	}
	compressions := []struct {
		name string
		opts []BuildOption
	}{
		{"none", nil},
		{"lz4", []BuildOption{WithCompression(CompressionAutoLZ4)}},
		{"zstd", []BuildOption{WithCompression(CompressionZstd)}},
	}
	for _, compression := range compressions {
		t.Run(compression.name, func(t *testing.T) {
			for _, file := range files {
				t.Run(file.name, func(t *testing.T) {
					data := lazyContent(file.size)
					compareLazyBuilds(t, compression.opts, map[string][]byte{"/file": data}, false)
				})
			}
			t.Run("mixed tree", func(t *testing.T) {
				contents := make(map[string][]byte)
				for _, file := range files {
					contents["/nested/"+strings.ReplaceAll(file.name, " ", "-")] = lazyContent(file.size)
				}
				compareLazyBuilds(t, compression.opts, contents, true)
			})
		})
	}
}

func compareLazyBuilds(t *testing.T, compression []BuildOption, files map[string][]byte, symlink bool) {
	t.Helper()
	var images [2]*writerAtBuffer
	for variant := range images {
		images[variant] = newWriterAtBuffer(8192)
		opts := append([]BuildOption{WithBlockSize(12), WithEpoch(lazyEpoch)}, compression...)
		b := NewBuilder(images[variant], opts...)
		if err := b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: lazyEpoch}); err != nil {
			t.Fatal(err)
		}
		if symlink {
			if err := b.AddDir("/nested", &staticFileInfo{name: "nested", mode: fs.ModeDir | 0o755, mod: lazyEpoch}); err != nil {
				t.Fatal(err)
			}
			if err := b.AddSymlink("/link", "nested/tiny", &staticFileInfo{name: "link", mode: fs.ModeSymlink | 0o777, mod: lazyEpoch}); err != nil {
				t.Fatal(err)
			}
		}
		for p, data := range files {
			if variant == 0 {
				if err := b.AddFile(p, lazyInfo(p, len(data)), data); err != nil {
					t.Fatal(err)
				}
			} else {
				data := data
				opens := 0
				if err := b.AddFileFunc(p, lazyInfo(p, len(data)), int64(len(data)), func() (io.ReadCloser, error) {
					opens++
					return io.NopCloser(bytes.NewReader(data)), nil
				}); err != nil {
					t.Fatal(err)
				}
				// Each source remains untouched until Build; exact counts are tested below.
				if opens != 0 {
					t.Fatalf("open called during AddFileFunc for %s", p)
				}
			}
		}
		if err := b.Build(); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(images[0].Bytes(), images[1].Bytes()) {
		t.Fatalf("AddFileFunc image differs from AddFile image")
	}
	result, err := Validate(bytes.NewReader(images[1].Bytes()), int64(len(images[1].Bytes())))
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Validate: %v, errors: %v", err, result.Errors)
	}
	image, err := Open(bytes.NewReader(images[1].Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range files {
		got, err := fs.ReadFile(image, strings.TrimPrefix(p, "/"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read %s: %v, equal: %t", p, err, bytes.Equal(got, want))
		}
	}
	if symlink {
		got, err := image.ReadLink("link")
		if err != nil || got != "nested/tiny" {
			t.Fatalf("ReadLink: %q, %v", got, err)
		}
	}
}

func TestAddFileFuncSizeErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		data string
		want int
	}{
		{"short", 5, "abcd", 4},
		{"long", 5, "abcdef", 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(newWriterAtBuffer(8192), WithEpoch(lazyEpoch))
			if err := b.AddFileFunc("/file", lazyInfo("file", int(tt.size)), tt.size, func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(tt.data)), nil
			}); err != nil {
				t.Fatal(err)
			}
			err := b.Build()
			if err == nil || !strings.Contains(err.Error(), "/file") || !strings.Contains(err.Error(), fmt.Sprintf("declared size %d, read %d", tt.size, tt.want)) {
				t.Fatalf("Build error = %v", err)
			}
		})
	}
}

func TestAddFileFuncOpenError(t *testing.T) {
	want := errors.New("source unavailable")
	b := NewBuilder(newWriterAtBuffer(8192))
	if err := b.AddFileFunc("/file", lazyInfo("file", 1), 1, func() (io.ReadCloser, error) { return nil, want }); err != nil {
		t.Fatal(err)
	}
	err := b.Build()
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "/file") {
		t.Fatalf("Build error = %v", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestAddFileFuncReadError(t *testing.T) {
	want := errors.New("source failed")
	b := NewBuilder(newWriterAtBuffer(8192))
	if err := b.AddFileFunc("/file", lazyInfo("file", 1), 1, func() (io.ReadCloser, error) {
		return io.NopCloser(failingReader{want}), nil
	}); err != nil {
		t.Fatal(err)
	}
	err := b.Build()
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "/file") {
		t.Fatalf("Build error = %v", err)
	}
}

type trackedReader struct {
	io.Reader
	open *int
}

func (r *trackedReader) Close() error { *r.open--; return nil }

func TestAddFileFuncOpensOnceAndOneReaderOpen(t *testing.T) {
	b := NewBuilder(newWriterAtBuffer(8192))
	opened, active, maxActive := 0, 0, 0
	var order []string
	for _, p := range []string{"/z", "/a", "/empty"} {
		p := p
		size := int64(1)
		if p == "/empty" {
			size = 0
		}
		if err := b.AddFileFunc(p, lazyInfo(p, int(size)), size, func() (io.ReadCloser, error) {
			opened++
			order = append(order, p)
			active++
			if active > maxActive {
				maxActive = active
			}
			return &trackedReader{Reader: strings.NewReader("x"), open: &active}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if opened != 0 {
		t.Fatalf("opened before Build: %d", opened)
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if opened != 2 || active != 0 || maxActive != 1 {
		t.Fatalf("opened=%d active=%d max=%d", opened, active, maxActive)
	}
	if got := strings.Join(order, ","); got != "/a,/z" {
		t.Fatalf("open order = %q, want /a,/z", got)
	}
}

type discardWriterAt struct{}

func (discardWriterAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func TestAddFileFuncMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("1 GiB streaming allocation test")
	}
	b := NewBuilder(discardWriterAt{}, WithEpoch(lazyEpoch))
	for i := range 1024 {
		p := fmt.Sprintf("/file-%04d", i)
		if err := b.AddFileFunc(p, lazyInfo(p, 1<<20), 1<<20, func() (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(zeroReader{}, 1<<20)), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta >= 64<<20 {
		t.Fatalf("Build allocated %d bytes, want less than 64 MiB", delta)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestAddFromFSLazy(t *testing.T) {
	files := fstest.MapFS{
		"file":         &fstest.MapFile{Data: []byte("hello"), Mode: 0o644, ModTime: lazyEpoch},
		"nested/other": &fstest.MapFile{Data: []byte("world"), Mode: 0o600, ModTime: lazyEpoch},
	}
	actual := newWriterAtBuffer(8192)
	b := NewBuilder(actual, WithEpoch(lazyEpoch))
	if err := b.AddFromFS(files); err != nil {
		t.Fatal(err)
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/addfromfs-v0.6.1.img")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), want) {
		t.Fatal("AddFromFS image differs from v0.6.1 golden image")
	}
}

func TestAddFromFSSizeChanged(t *testing.T) {
	files := fstest.MapFS{"file": &fstest.MapFile{Data: []byte("hello"), Mode: 0o644}}
	b := NewBuilder(newWriterAtBuffer(8192))
	if err := b.AddFromFS(files); err != nil {
		t.Fatal(err)
	}
	files["file"].Data = []byte("hi")
	err := b.Build()
	if err == nil || !strings.Contains(err.Error(), "/file") || !strings.Contains(err.Error(), "declared size 5, read 2") {
		t.Fatalf("Build error = %v", err)
	}
}
