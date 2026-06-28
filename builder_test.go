package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// writerAtBuffer wraps a byte slice to implement io.WriterAt and io.ReaderAt.
type writerAtBuffer struct {
	data []byte
}

func newWriterAtBuffer(size int) *writerAtBuffer {
	return &writerAtBuffer{data: make([]byte, size)}
}

func (w *writerAtBuffer) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(w.data) {
		// Grow the buffer.
		grown := make([]byte, end)
		copy(grown, w.data)
		w.data = grown
	}
	return copy(w.data[off:], p), nil
}

func (w *writerAtBuffer) ReadAt(p []byte, off int64) (int, error) {
	if int(off) >= len(w.data) {
		return 0, fs.ErrNotExist
	}
	n := copy(p, w.data[off:])
	return n, nil
}

func (w *writerAtBuffer) Bytes() []byte {
	return w.data
}

func TestBuilderRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(64 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))

	// Add root directory.
	b.AddDir("/", &staticFileInfo{
		name: "/",
		mode: fs.ModeDir | 0o755,
		mod:  epoch,
	})

	// Add a subdirectory.
	b.AddDir("/etc", &staticFileInfo{
		name: "etc",
		mode: fs.ModeDir | 0o755,
		mod:  epoch,
	})

	// Add a regular file.
	fileContent := []byte("nameserver 8.8.8.8\n")
	b.AddFile("/etc/resolv.conf", &staticFileInfo{
		name: "resolv.conf",
		mode: 0o644,
		size: int64(len(fileContent)),
		mod:  epoch,
	}, fileContent)

	// Add a symlink.
	b.AddSymlink("/etc/link", "resolv.conf", &staticFileInfo{
		name: "link",
		mode: fs.ModeSymlink | 0o777,
		mod:  epoch,
	})

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Open the image with the reader.
	img := bytes.NewReader(buf.Bytes())
	fsys, err := Open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Check root directory.
	rootInfo, err := fsys.Stat(".")
	if err != nil {
		t.Fatalf("Stat(.): %v", err)
	}
	if !rootInfo.IsDir() {
		t.Error("root is not a directory")
	}

	// Read directory entries.
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}

	found := make(map[string]bool)
	for _, e := range entries {
		found[e.Name()] = true
	}
	if !found["etc"] {
		t.Error("missing 'etc' in root directory listing")
	}

	// Check subdirectory.
	etcInfo, err := fsys.Stat("etc")
	if err != nil {
		t.Fatalf("Stat(etc): %v", err)
	}
	if !etcInfo.IsDir() {
		t.Error("etc is not a directory")
	}

	// Read etc directory.
	etcEntries, err := fs.ReadDir(fsys, "etc")
	if err != nil {
		t.Fatalf("ReadDir(etc): %v", err)
	}
	etcFound := make(map[string]bool)
	for _, e := range etcEntries {
		etcFound[e.Name()] = true
	}
	if !etcFound["resolv.conf"] {
		t.Error("missing 'resolv.conf' in etc directory listing")
	}
	if !etcFound["link"] {
		t.Error("missing 'link' in etc directory listing")
	}

	// Read the file.
	data, err := fs.ReadFile(fsys, "etc/resolv.conf")
	if err != nil {
		t.Fatalf("ReadFile(etc/resolv.conf): %v", err)
	}
	if string(data) != "nameserver 8.8.8.8\n" {
		t.Errorf("file content = %q, want %q", string(data), "nameserver 8.8.8.8\n")
	}

	// Read the symlink.
	target, err := fsys.ReadLink("etc/link")
	if err != nil {
		t.Fatalf("ReadLink(etc/link): %v", err)
	}
	if target != "resolv.conf" {
		t.Errorf("symlink target = %q, want %q", target, "resolv.conf")
	}
}

func TestBuilderLargeFile(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))

	// Create a file larger than one block to test FLAT_PLAIN layout.
	largeContent := make([]byte, 8192)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}

	b.AddDir("/", &staticFileInfo{
		name: "/",
		mode: fs.ModeDir | 0o755,
		mod:  epoch,
	})

	b.AddFile("/bigfile", &staticFileInfo{
		name: "bigfile",
		mode: 0o644,
		size: int64(len(largeContent)),
		mod:  epoch,
	}, largeContent)

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := bytes.NewReader(buf.Bytes())
	fsys, err := Open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	data, err := fs.ReadFile(fsys, "bigfile")
	if err != nil {
		t.Fatalf("ReadFile(bigfile): %v", err)
	}
	if !bytes.Equal(data, largeContent) {
		t.Errorf("large file content mismatch: got %d bytes, want %d", len(data), len(largeContent))
	}
}

func TestBuilderEmptyImage(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(8192)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))

	// Build with no explicit entries; root should be auto-created.
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := bytes.NewReader(buf.Bytes())
	fsys, err := Open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	info, err := fsys.Stat(".")
	if err != nil {
		t.Fatalf("Stat(.): %v", err)
	}
	if !info.IsDir() {
		t.Error("root is not a directory")
	}
}

func TestBuilderAddFromFS(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Build a small image manually first.
	buf1 := newWriterAtBuffer(64 * 1024)
	b1 := NewBuilder(buf1, WithBlockSize(12), WithEpoch(epoch))

	b1.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	b1.AddDir("/sub", &staticFileInfo{name: "sub", mode: fs.ModeDir | 0o755, mod: epoch})
	b1.AddFile("/sub/hello.txt", &staticFileInfo{
		name: "hello.txt", mode: 0o644, size: 6, mod: epoch,
	}, []byte("hello\n"))

	if err := b1.Build(); err != nil {
		t.Fatalf("Build source image: %v", err)
	}

	// Open the built image as a source fs.FS.
	srcImg := bytes.NewReader(buf1.Bytes())
	srcFS, err := Open(srcImg)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}

	// Build a new image from the source FS.
	buf2 := newWriterAtBuffer(64 * 1024)
	b2 := NewBuilder(buf2, WithBlockSize(12), WithEpoch(epoch))
	if err := b2.AddFromFS(srcFS); err != nil {
		t.Fatalf("AddFromFS: %v", err)
	}
	if err := b2.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Verify the round-tripped image.
	dstImg := bytes.NewReader(buf2.Bytes())
	dstFS, err := Open(dstImg)
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}

	data, err := fs.ReadFile(dstFS, "sub/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile(sub/hello.txt): %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("content = %q, want %q", string(data), "hello\n")
	}
}

func TestBuilderDeepNesting(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(64 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))

	mkDir := func(p string) {
		b.AddDir(p, &staticFileInfo{name: p, mode: fs.ModeDir | 0o755, mod: epoch})
	}
	mkFile := func(p string, data string) {
		b.AddFile(p, &staticFileInfo{name: p, mode: 0o644, size: int64(len(data)), mod: epoch}, []byte(data))
	}

	mkDir("/")
	mkDir("/a")
	mkDir("/a/b")
	mkDir("/a/b/c")
	mkFile("/a/b/c/deep.txt", "deep content\n")
	mkFile("/a/top.txt", "top content\n")

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := bytes.NewReader(buf.Bytes())
	fsys, err := Open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Read deeply nested file.
	data, err := fs.ReadFile(fsys, "a/b/c/deep.txt")
	if err != nil {
		t.Fatalf("ReadFile(a/b/c/deep.txt): %v", err)
	}
	if string(data) != "deep content\n" {
		t.Errorf("deep content = %q, want %q", string(data), "deep content\n")
	}

	// Read file at intermediate level.
	data2, err := fs.ReadFile(fsys, "a/top.txt")
	if err != nil {
		t.Fatalf("ReadFile(a/top.txt): %v", err)
	}
	if string(data2) != "top content\n" {
		t.Errorf("top content = %q, want %q", string(data2), "top content\n")
	}

	// Walk and count entries.
	count := 0
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	// root, a, a/b, a/b/c, a/b/c/deep.txt, a/top.txt = 6
	if count != 6 {
		t.Errorf("WalkDir count = %d, want 6", count)
	}
}

func TestBuilderManyFiles(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// Add 50 files to test directory packing with many entries.
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("/file_%03d.txt", i)
		content := fmt.Sprintf("content of file %d\n", i)
		b.AddFile(name, &staticFileInfo{
			name: name, mode: 0o644, size: int64(len(content)), mod: epoch,
		}, []byte(content))
	}

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := bytes.NewReader(buf.Bytes())
	fsys, err := Open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 50 {
		t.Errorf("entry count = %d, want 50", len(entries))
	}

	// Spot check a few files.
	for _, i := range []int{0, 25, 49} {
		name := fmt.Sprintf("file_%03d.txt", i)
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		want := fmt.Sprintf("content of file %d\n", i)
		if string(data) != want {
			t.Errorf("%s content = %q, want %q", name, string(data), want)
		}
	}
}

func TestBuilderCompression(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))

	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// Create a large compressible file (repeated text compresses well).
	var bigContent bytes.Buffer
	for range 500 {
		bigContent.WriteString("This is a repeating line of text that should compress very well with LZ4.\n")
	}
	data := bigContent.Bytes()

	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt",
		mode: 0o644,
		size: int64(len(data)),
		mod:  epoch,
	}, data)

	// Also add a small file (should stay uncompressed/inline).
	small := []byte("hello world\n")
	b.AddFile("/small.txt", &staticFileInfo{
		name: "small.txt",
		mode: 0o644,
		size: int64(len(small)),
		mod:  epoch,
	}, small)

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Verify we can read the image.
	fsys, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Read back the big file.
	readBack, err := fs.ReadFile(fsys, "big.txt")
	if err != nil {
		t.Fatalf("ReadFile(big.txt): %v", err)
	}
	if !bytes.Equal(readBack, data) {
		t.Errorf("big.txt: content mismatch (got %d bytes, want %d bytes)", len(readBack), len(data))
	}

	// Read back the small file.
	readSmall, err := fs.ReadFile(fsys, "small.txt")
	if err != nil {
		t.Fatalf("ReadFile(small.txt): %v", err)
	}
	if string(readSmall) != string(small) {
		t.Errorf("small.txt: content = %q, want %q", string(readSmall), string(small))
	}

	t.Logf("big.txt: %d bytes original, image size: %d bytes", len(data), len(buf.Bytes()))
}

func TestBuilderZstdRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionZstd))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	var bigContent bytes.Buffer
	for range 500 {
		bigContent.WriteString("This is a repeating line of text that should compress very well with zstd.\n")
	}
	data := bigContent.Bytes()
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	fsys, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	readBack, err := fs.ReadFile(fsys, "big.txt")
	if err != nil {
		t.Fatalf("ReadFile(big.txt): %v", err)
	}
	if !bytes.Equal(readBack, data) {
		t.Fatalf("big.txt: content mismatch (got %d bytes, want %d)", len(readBack), len(data))
	}
}

func TestBuilderZstdAvailComprAlgs(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionZstd))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	data := bytes.Repeat([]byte("compress me with zstd please\n"), 500)
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := buf.Bytes()
	// available_compr_algs is a __le16 at superblock offset 84
	// (superblock starts at byte 1024).
	got := binary.LittleEndian.Uint16(img[1024+84:])
	want := uint16(1) << ondisk.CompressionZstd
	if got != want {
		t.Fatalf("available_compr_algs = 0x%04x, want 0x%04x", got, want)
	}
}

// staticFileInfo implements fs.FileInfo for testing.
type staticFileInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (fi *staticFileInfo) Name() string       { return fi.name }
func (fi *staticFileInfo) Size() int64        { return fi.size }
func (fi *staticFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *staticFileInfo) ModTime() time.Time { return fi.mod }
func (fi *staticFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *staticFileInfo) Sys() any           { return nil }
