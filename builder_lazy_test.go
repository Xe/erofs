package erofs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
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

// lazyText returns size bytes of compressible text.
func lazyText(size int) []byte {
	words := []string{"erofs", "block", "inode", "cluster", "stream", "spool", "builder", "image", "reader", "kernel"}
	rng := rand.New(rand.NewPCG(1, uint64(size)))
	var buf bytes.Buffer
	for buf.Len() < size {
		buf.WriteString(words[rng.IntN(len(words))])
		if rng.IntN(12) == 0 {
			buf.WriteByte('\n')
		} else {
			buf.WriteByte(' ')
		}
	}
	return buf.Bytes()[:size]
}

// lazyRandom returns size bytes that do not compress.
func lazyRandom(size int) []byte {
	rng := rand.NewChaCha8([32]byte{byte(size), byte(size >> 8), byte(size >> 16)})
	data := make([]byte, size)
	_, _ = rng.Read(data)
	return data
}

var lazyCompressions = []struct {
	name string
	opts []BuildOption
}{
	{"none", nil},
	{"lz4", []BuildOption{WithCompression(CompressionAutoLZ4)}},
	{"zstd", []BuildOption{WithCompression(CompressionZstd)}},
}

func TestAddFileFuncMatchesAddFile(t *testing.T) {
	files := []struct {
		name string
		path string
		data []byte
	}{
		{"empty", "/empty", nil},
		{"tiny", "/tiny", lazyText(1)},
		{"max inline", "/max-inline", lazyText(4096 - 64)},
		{"over inline", "/over-inline", lazyText(4096 - 63)},
		{"one block", "/one-block", lazyText(4096)},
		{"block plus one", "/block-plus-one", lazyText(4097)},
		{"multi block", "/multi-block", lazyText(3*4096 + 17)},
		{"random", "/random.bin", lazyRandom(1<<20 + 5)},
		{"mixed groups", "/mixed-groups", append(lazyText(64<<10), lazyRandom(64<<10+9)...)},
		{"known extension", "/known.png", lazyText(64 << 10)},
		{"large", "/large", lazyText(10<<20 + 5)},
	}
	for _, compression := range lazyCompressions {
		t.Run(compression.name, func(t *testing.T) {
			for _, file := range files {
				t.Run(file.name, func(t *testing.T) {
					compareLazyBuilds(t, compression.opts, map[string][]byte{file.path: file.data}, false)
				})
			}
			t.Run("mixed tree", func(t *testing.T) {
				contents := make(map[string][]byte)
				for i, file := range files {
					dir := "/nested"
					if i%2 == 1 {
						dir = "/nested/deeper"
					}
					contents[dir+file.path] = file.data
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
			if err := b.AddSymlink("/link", "nested/deeper/tiny", &staticFileInfo{name: "link", mode: fs.ModeSymlink | 0o777, mod: lazyEpoch}); err != nil {
				t.Fatal(err)
			}
			if err := b.AddSymlink("/nested/deeper/up", "../../link", &staticFileInfo{name: "up", mode: fs.ModeSymlink | 0o777, mod: lazyEpoch}); err != nil {
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
		if err != nil || got != "nested/deeper/tiny" {
			t.Fatalf("ReadLink: %q, %v", got, err)
		}
		data, err := fs.ReadFile(image, "nested/deeper/up")
		if err != nil || !bytes.Equal(data, files["/nested/deeper/tiny"]) {
			t.Fatalf("read through symlinks: %v, equal: %t", err, bytes.Equal(data, files["/nested/deeper/tiny"]))
		}
	}
}

// lazySourceKinds are the two kinds of lazy file: one that the builder opens
// at step 7 (flat-only), and one that it opens at step 3b to try compression
// (candidate).
var lazySourceKinds = []struct {
	name string
	size int
	opts []BuildOption
}{
	{"flat-only", 5, nil},
	{"flat-only zstd", 5, []BuildOption{WithCompression(CompressionZstd)}},
	{"candidate lz4", 3*4096 + 17, []BuildOption{WithCompression(CompressionAutoLZ4)}},
	{"candidate zstd", 3*4096 + 17, []BuildOption{WithCompression(CompressionZstd)}},
}

func TestAddFileFuncSizeErrors(t *testing.T) {
	for _, kind := range lazySourceKinds {
		for _, tt := range []struct {
			name  string
			delta int
		}{
			{"short", -1},
			{"long", 1},
		} {
			t.Run(kind.name+"/"+tt.name, func(t *testing.T) {
				data := lazyText(kind.size + tt.delta)
				opts := append([]BuildOption{WithEpoch(lazyEpoch), WithSpoolDir(t.TempDir())}, kind.opts...)
				b := NewBuilder(newWriterAtBuffer(8192), opts...)
				if err := b.AddFileFunc("/file", lazyInfo("file", kind.size), int64(kind.size), func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(data)), nil
				}); err != nil {
					t.Fatal(err)
				}
				err := b.Build()
				want := fmt.Sprintf("declared size %d, read %d", kind.size, len(data))
				if err == nil || !strings.Contains(err.Error(), "/file") || !strings.Contains(err.Error(), want) {
					t.Fatalf("Build error = %v, want it to contain /file and %q", err, want)
				}
			})
		}
	}
}

func TestAddFileFuncOpenError(t *testing.T) {
	for _, kind := range lazySourceKinds {
		t.Run(kind.name, func(t *testing.T) {
			want := errors.New("source unavailable")
			opts := append([]BuildOption{WithSpoolDir(t.TempDir())}, kind.opts...)
			b := NewBuilder(newWriterAtBuffer(8192), opts...)
			if err := b.AddFileFunc("/file", lazyInfo("file", kind.size), int64(kind.size), func() (io.ReadCloser, error) { return nil, want }); err != nil {
				t.Fatal(err)
			}
			err := b.Build()
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "/file") {
				t.Fatalf("Build error = %v", err)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestAddFileFuncReadError(t *testing.T) {
	for _, kind := range lazySourceKinds {
		t.Run(kind.name, func(t *testing.T) {
			want := errors.New("source failed")
			opts := append([]BuildOption{WithSpoolDir(t.TempDir())}, kind.opts...)
			b := NewBuilder(newWriterAtBuffer(8192), opts...)
			if err := b.AddFileFunc("/file", lazyInfo("file", kind.size), int64(kind.size), func() (io.ReadCloser, error) {
				return io.NopCloser(failingReader{want}), nil
			}); err != nil {
				t.Fatal(err)
			}
			err := b.Build()
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "/file") {
				t.Fatalf("Build error = %v", err)
			}
		})
	}
}

type trackedReader struct {
	io.Reader
	open *int
}

func (r *trackedReader) Close() error { *r.open--; return nil }

func TestAddFileFuncOpensOnceAndOneReaderOpen(t *testing.T) {
	files := map[string][]byte{
		"/z":          lazyText(1),             // flat-only
		"/a":          lazyText(1),             // flat-only
		"/empty":      nil,                     // never opened
		"/text":       lazyText(3*4096 + 17),   // candidate, compressed
		"/random.bin": lazyRandom(3*4096 + 17), // candidate, flat fallback
		"/photo.png":  lazyText(3*4096 + 17),   // flat-only by extension
		"/mixed":      append(lazyText(64<<10), lazyRandom(64<<10)...),
	}
	for _, compression := range lazyCompressions {
		t.Run(compression.name, func(t *testing.T) {
			opts := append([]BuildOption{WithSpoolDir(t.TempDir())}, compression.opts...)
			b := NewBuilder(newWriterAtBuffer(8192), opts...)
			opens := make(map[string]int)
			active, maxActive := 0, 0
			var order []string
			for p, data := range files {
				if err := b.AddFileFunc(p, lazyInfo(p, len(data)), int64(len(data)), func() (io.ReadCloser, error) {
					opens[p]++
					order = append(order, p)
					active++
					maxActive = max(maxActive, active)
					return &trackedReader{Reader: bytes.NewReader(data), open: &active}, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if len(opens) != 0 {
				t.Fatalf("opened before Build: %v", opens)
			}
			if err := b.Build(); err != nil {
				t.Fatal(err)
			}
			for p, data := range files {
				want := 1
				if len(data) == 0 {
					want = 0
				}
				if opens[p] != want {
					t.Errorf("%s opened %d times, want %d", p, opens[p], want)
				}
			}
			if active != 0 || maxActive != 1 {
				t.Fatalf("active=%d max=%d", active, maxActive)
			}
			if compression.opts == nil {
				if got := strings.Join(order, ","); got != "/a,/mixed,/photo.png,/random.bin,/text,/z" {
					t.Fatalf("open order = %q, want inode order", got)
				}
			}
		})
	}
}

func TestSpoolRemoved(t *testing.T) {
	sourceErr := errors.New("source failed")
	for _, tt := range []struct {
		name    string
		failAt  string // path whose reader fails, or ""
		wantErr bool
	}{
		{"success", "", false},
		{"candidate read error", "/b-text", true},
		{"flat read error after spool", "/z-tiny", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			b := NewBuilder(newWriterAtBuffer(8192), WithCompression(CompressionZstd), WithSpoolDir(dir))
			spoolFiles := -1
			files := map[string][]byte{
				"/a-text": lazyText(3*4096 + 17),
				"/b-text": lazyText(5*4096 + 1),
				"/c-rand": lazyRandom(2*4096 + 3),
				"/z-tiny": lazyText(10),
			}
			for p, data := range files {
				if err := b.AddFileFunc(p, lazyInfo(p, len(data)), int64(len(data)), func() (io.ReadCloser, error) {
					if p == "/b-text" {
						// The spool of /a-text must be in dir by now.
						entries, err := os.ReadDir(dir)
						if err != nil {
							return nil, err
						}
						spoolFiles = len(entries)
					}
					if p == tt.failAt {
						return io.NopCloser(failingReader{sourceErr}), nil
					}
					return io.NopCloser(bytes.NewReader(data)), nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			err := b.Build()
			if tt.wantErr != (err != nil) {
				t.Fatalf("Build error = %v, want error: %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, sourceErr) {
				t.Fatalf("Build error = %v, want %v", err, sourceErr)
			}
			if spoolFiles != 1 {
				t.Fatalf("spool directory had %d files during Build, want 1", spoolFiles)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("spool directory not empty after Build: %v", entries)
			}
		})
	}
}

type discardWriterAt struct{}

func (discardWriterAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

// textReader generates compressible text without holding it in memory.
type textReader struct {
	n   int64 // bytes left
	pos int
}

const textReaderLine = "the builder streams each file through one group buffer and a spool\n"

func (r *textReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = textReaderLine[r.pos]
		r.pos = (r.pos + 1) % len(textReaderLine)
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// TestAddFileFuncMemory is the acceptance test for streaming: the live heap
// during Build must not depend on file content.
func TestAddFileFuncMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("1 GiB streaming memory test")
	}
	for _, compression := range []struct {
		name string
		opts []BuildOption
	}{
		{"none", nil},
		{"zstd", []BuildOption{WithCompression(CompressionZstd)}},
	} {
		t.Run(compression.name, func(t *testing.T) {
			opts := append([]BuildOption{WithEpoch(lazyEpoch), WithSpoolDir(t.TempDir())}, compression.opts...)
			b := NewBuilder(discardWriterAt{}, opts...)
			var peak uint64
			opens := 0
			for i := range 1024 {
				p := fmt.Sprintf("/dir-%02d/file-%04d.txt", i%32, i)
				if err := b.AddFileFunc(p, lazyInfo(p, 1<<20), 1<<20, func() (io.ReadCloser, error) {
					opens++
					runtime.GC()
					var ms runtime.MemStats
					runtime.ReadMemStats(&ms)
					peak = max(peak, ms.HeapAlloc)
					return io.NopCloser(&textReader{n: 1 << 20}), nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Build(); err != nil {
				t.Fatal(err)
			}
			if opens != 1024 {
				t.Fatalf("opens = %d, want 1024", opens)
			}
			t.Logf("peak live heap: %.1f MiB", float64(peak)/(1<<20))
			if peak >= 64<<20 {
				t.Fatalf("peak live heap %d bytes, want less than 64 MiB", peak)
			}
		})
	}
}

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
