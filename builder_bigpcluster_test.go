package erofs

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// blocksLo reads the superblock BlocksLo (__le32 at superblock offset 36).
func blocksLo(img []byte) uint32 {
	return binary.LittleEndian.Uint32(img[1024+36:])
}

// findCompressed returns the compressedFileData for the named inode.
func findCompressed(t *testing.T, b *Builder, path string) *compressedFileData {
	t.Helper()
	for ino, cdata := range b.compressedData {
		if ino.path == path {
			return cdata
		}
	}
	t.Fatalf("%s was not stored as a compressed inode", path)
	return nil
}

// TestBuilderBigPClusterEncoding asserts the on-disk index encoding for a single
// big pcluster with a partial tail matches what the kernel expects.
func TestBuilderBigPClusterEncoding(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(1 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// One pcluster: 40000 bytes is 10 lclusters, under the default 16-lcluster
	// pcluster span, and ends mid-lcluster (tail marker). 40000 % 4096 = 2752.
	const fsize = 40000
	data := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog.\n"), 1000)[:fsize]
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	cdata := findCompressed(t, b, "/big.txt")
	if len(cdata.indexEntries) != (fsize+4095)/4096 { // ceil(40000/4096) = 10
		t.Fatalf("indexEntries = %d, want %d", len(cdata.indexEntries), (fsize+4095)/4096)
	}
	if len(cdata.blocks) >= len(cdata.indexEntries) {
		t.Fatalf("blocks = %d, want < %d (no packing happened)", len(cdata.blocks), len(cdata.indexEntries))
	}
	e := cdata.indexEntries
	if e[0].Type() != ondisk.LClusterTypeHead1 {
		t.Fatalf("entry 0 type = %d, want HEAD1", e[0].Type())
	}
	if e[1].Type() != ondisk.LClusterTypeNonHead || e[1].Delta0()&ondisk.LID0CBlkCnt == 0 {
		t.Fatalf("entry 1 must be NONHEAD with D0_CBLKCNT, got type=%d delta0=0x%x", e[1].Type(), e[1].Delta0())
	}
	if cblk := e[1].Delta0() &^ uint16(ondisk.LID0CBlkCnt); int(cblk) != len(cdata.blocks) {
		t.Fatalf("cblkcnt = %d, want %d", cblk, len(cdata.blocks))
	}
	last := e[len(e)-1]
	if last.Type() != ondisk.LClusterTypePlain || last.ClusterOfs != uint16(fsize%4096) {
		t.Fatalf("tail marker = type %d clusterofs %d, want PLAIN %d", last.Type(), last.ClusterOfs, fsize%4096)
	}
}

// TestBuilderBigPClusterSavesSpace is the issue #5 regression test: a compressible
// file must produce fewer data blocks than the uncompressed build.
func TestBuilderBigPClusterSavesSpace(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	data := bytes.Repeat([]byte("highly compressible content for the size regression test\n"), 40000)

	for _, alg := range []CompressionAlgorithm{CompressionAutoLZ4, CompressionZstd} {
		build := func(opts ...BuildOption) []byte {
			buf := newWriterAtBuffer(8 << 20)
			base := []BuildOption{WithBlockSize(12), WithEpoch(epoch)}
			b := NewBuilder(buf, append(base, opts...)...)
			b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
			b.AddFile("/big.txt", &staticFileInfo{
				name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
			}, data)
			if err := b.Build(); err != nil {
				t.Fatalf("Build: %v", err)
			}
			return buf.Bytes()
		}
		uBlk := blocksLo(build())
		cBlk := blocksLo(build(WithCompression(alg)))
		if cBlk >= uBlk {
			t.Errorf("alg %d: compressed BlocksLo %d not < uncompressed %d", alg, cBlk, uBlk)
		}
		t.Logf("alg %d: uncompressed=%d blocks, compressed=%d blocks", alg, uBlk, cBlk)
	}
}

// TestBuilderBigPClusterRoundTrip round-trips block-aligned, unaligned (tail
// marker), and multi-pcluster files through the reader for both algorithms.
func TestBuilderBigPClusterRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	mkdata := func() map[string][]byte {
		return map[string][]byte{
			"/aligned.txt":   bytes.Repeat([]byte("0123456789abcdef"), 4096), // 64 KiB exactly
			"/unaligned.txt": bytes.Repeat([]byte("compress me well\n"), 9000),
			"/multi.txt":     bytes.Repeat([]byte("many pclusters worth of repeating text\n"), 60000),
		}
	}
	for _, alg := range []CompressionAlgorithm{CompressionAutoLZ4, CompressionZstd} {
		files := mkdata()
		buf := newWriterAtBuffer(16 << 20)
		b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(alg))
		b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
		for name, data := range files {
			b.AddFile(name, &staticFileInfo{name: name[1:], mode: 0o644, size: int64(len(data)), mod: epoch}, data)
		}
		if err := b.Build(); err != nil {
			t.Fatalf("alg %d Build: %v", alg, err)
		}
		fsys, err := Open(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("alg %d Open: %v", alg, err)
		}
		for name, want := range files {
			got, err := fs.ReadFile(fsys, name[1:])
			if err != nil {
				t.Fatalf("alg %d ReadFile(%s): %v", alg, name, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("alg %d %s: mismatch (got %d, want %d)", alg, name, len(got), len(want))
			}
		}
	}
}

// TestBuilderBigPClusterMixed forces small (2-lcluster) pclusters so one inode
// holds both a compressed big pcluster (HEAD1+NONHEAD) and an incompressible
// PLAIN group, then round-trips it.
func TestBuilderBigPClusterMixed(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(1 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch),
		WithCompression(CompressionAutoLZ4), WithPClusterSize(13)) // 2 lclusters
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	rng := rand.New(rand.NewSource(1))
	var data bytes.Buffer
	data.Write(bytes.Repeat([]byte("compressible\n"), 700)[:8192]) // 2 blocks, compresses
	rnd := make([]byte, 8192)
	rng.Read(rnd)
	data.Write(rnd) // 2 blocks, incompressible
	content := data.Bytes()

	b.AddFile("/mixed.bin", &staticFileInfo{name: "mixed.bin", mode: 0o644, size: int64(len(content)), mod: epoch}, content)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	cdata := findCompressed(t, b, "/mixed.bin")
	var head1, nonhead, plain int
	for _, e := range cdata.indexEntries {
		switch e.Type() {
		case ondisk.LClusterTypeHead1:
			head1++
		case ondisk.LClusterTypeNonHead:
			nonhead++
		case ondisk.LClusterTypePlain:
			plain++
		}
	}
	if head1 == 0 || nonhead == 0 || plain == 0 {
		t.Fatalf("want HEAD1+NONHEAD+PLAIN, got head1=%d nonhead=%d plain=%d", head1, nonhead, plain)
	}

	fsys, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := fs.ReadFile(fsys, "mixed.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("mixed.bin: content mismatch")
	}
}

// TestBuilderBigPClusterFsck validates builder-produced LZ4 big-pcluster images
// against the reference fsck.erofs (skips if unavailable). fsck --extract fully
// decompresses every file with liblz4, so a clean run plus byte-exact extracted
// output validates the index encoding and the EROFS 0-padding (right-aligned
// compressed stream) end-to-end.
func TestBuilderBigPClusterFsck(t *testing.T) {
	fsck, err := exec.LookPath("fsck.erofs")
	if err != nil {
		t.Skip("fsck.erofs not installed")
	}
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(8 << 20)

	// Mix of layouts: block-aligned single pcluster, partial-tail single
	// pcluster, and a large multi-pcluster file.
	files := map[string][]byte{
		"/aligned.txt": bytes.Repeat([]byte("0123456789abcdef"), 4096),   // 64 KiB exactly
		"/tail.txt":    bytes.Repeat([]byte("compress me well\n"), 9000), // partial tail
		"/multi.txt":   bytes.Repeat([]byte("many pclusters of repeating text\n"), 30000),
	}
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	for name, data := range files {
		b.AddFile(name, &staticFileInfo{name: name[1:], mode: 0o644, mod: epoch}, data)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.img")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	xdir := filepath.Join(dir, "x")
	out, err := exec.Command(fsck, "--extract="+xdir, path).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck.erofs --extract failed: %v\n%s", err, out)
	}
	// Extracted bytes must match exactly (proves liblz4 decoded our blocks).
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(xdir, name[1:]))
		if err != nil {
			t.Fatalf("read extracted %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: fsck-extracted content mismatch (got %d, want %d)", name, len(got), len(want))
		}
	}
}
