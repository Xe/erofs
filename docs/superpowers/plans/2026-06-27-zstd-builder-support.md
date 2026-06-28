# Zstandard Builder Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the EROFS `Builder` able to emit Zstandard-compressed files (it currently only emits LZ4), and fix the reader so it can actually decode zstd pclusters.

**Architecture:** The reader already maps algorithm id 3 to `zstdDecompress`, but that function uses `zstd.DecodeAll`, which rejects the zero-padding EROFS leaves at the tail of a pcluster block. We rewrite it to a streaming reader (mirroring `deflateDecompress`). On the builder, compression is currently hardcoded to LZ4 in three places (`tryCompressFile`, the map-header `h_algorithmtype` byte, and `computeComprAlgs`); we add a `CompressionZstd` option and dispatch on it. For kernel/mkfs compatibility, zstd additionally requires the `compr_cfgs` superblock feature plus a `z_erofs_zstd_cfgs` config record, which forces a small shift in where inode metadata begins.

**Tech Stack:** Go, `github.com/klauspost/compress/zstd` (already a dependency, used by the reader), `github.com/pierrec/lz4/v4` (existing LZ4 path).

---

## Background facts (verified against the codebase before writing this plan)

- The reader is wired for zstd in both decompression paths (`compress.go:283` extended, `compress.go:473` compact) via `zstdDecompress` (`compress.go:526`). **But** `zstdDecompress` calls `decoder.DecodeAll(compressed, ...)` where `compressed` is the whole block-sized pcluster including the zero padding the builder writes. `DecodeAll` errors with `invalid input: magic number mismatch` on that trailing padding. A streaming `zstd.NewReader` + `io.ReadFull` of exactly `decompSize` bytes works (this was confirmed empirically). The existing `deflateDecompress` (`compress.go:541`) already uses exactly this streaming pattern.
- The builder compresses each lcluster independently with `h_clusterbits = 0`, so each compressed extent decompresses to exactly one block (`blockSize`). There are no big pclusters. This keeps the zstd window small and bounded.
- `tryCompressFile` (`builder_compress.go:58`) hardcodes `lz4.CompressBlock`.
- `writeCompressedInode` (`builder_compress.go:184`) hardcodes `mh[6] = ondisk.CompressionLZ4`.
- `computeComprAlgs` (`builder.go:956`) hardcodes `1 << ondisk.CompressionLZ4`.
- `assignNIDs` (`builder.go:544`) starts inode metadata at `alignUp(SuperOffset + 144, 32)` = 1184. zstd's `compr_cfgs` record must live in the gap right after the 144-byte superblock (offset 1168), so metadata must be pushed past it when cfgs are present.
- `ondisk.CompressionZstd = 3` and `ondisk.FeatureIncompatComprCfgs = 0x2` already exist (`internal/ondisk/constants.go:113`, `:31`).
- The CLI `cmd/mkfs.erofs/main.go:46` hardcodes `WithCompression(CompressionAutoLZ4)`.
- `builder.go` already imports `encoding/binary`; `compress.go` already imports `bytes` and `io`. No new imports needed except `github.com/klauspost/compress/zstd` in `builder_compress.go`.

### Validation limitation — READ THIS

The locally installed `mkfs.erofs` is **1.7.1, which has no zstd support** (zstd landed in erofs-utils 1.8). Therefore the on-disk `compr_cfgs` layout and feature flags in Task 7 **cannot be validated locally** against the reference tooling. Every task in this plan is verified by round-tripping through *this project's own reader*, which is fully sufficient for Tasks 1–6. Task 7 (kernel compatibility) is best-effort and must be flagged for out-of-band validation with `fsck.erofs`/a kernel mount from erofs-utils ≥ 1.8. Do not claim kernel compatibility is verified.

---

## File Structure

- `compress.go` — **Modify** `zstdDecompress` (`:526-539`) to stream-decode, tolerating trailing pcluster padding. Reader-side only.
- `builder_compress.go` — **Modify** to add the `CompressionZstd` constant, a lazily-created zstd encoder helper, algorithm dispatch in `tryCompressFile`, and the algorithm-id byte in `writeCompressedInode`.
- `builder.go` — **Modify** `computeComprAlgs`, `computeIncompatFeatures`, `assignNIDs`, add `comprAlgID`/`comprCfgsSize`/`writeComprCfgs` helpers, and call `writeComprCfgs` from `Build`. Add a `zstdEnc *zstd.Encoder` field to the `Builder` struct.
- `internal/ondisk/types.go` — **Modify** to add the `ZstdCfgs` struct (documentation/parity with `LZ4Cfgs`).
- `compress_zstd_test.go` — **Create** a focused unit test for the reader fix.
- `builder_test.go` — **Modify** to add a zstd round-trip integration test.
- `cmd/mkfs.erofs/main.go` — **Modify** to expose a `--compression` flag.
- `CLAUDE.md` — **Modify** the project description note (it currently overclaims full zstd support).

---

### Task 1: Fix the reader's zstd decompression to tolerate pcluster padding

**Files:**
- Modify: `compress.go:526-539`
- Test: `compress_zstd_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `compress_zstd_test.go`:

```go
package erofs

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestZstdDecompressToleratesPadding(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	orig := bytes.Repeat([]byte("erofs zstd pcluster padding\n"), 64)
	frame := enc.EncodeAll(orig, nil)

	// Simulate an EROFS pcluster: one block, zstd frame at the front,
	// remainder zero-padded -- exactly what the builder writes.
	block := make([]byte, 4096)
	copy(block, frame)

	dst := make([]byte, len(orig))
	n, err := zstdDecompress(block, dst)
	if err != nil {
		t.Fatalf("zstdDecompress: %v", err)
	}
	if n != len(orig) {
		t.Fatalf("n = %d, want %d", n, len(orig))
	}
	if !bytes.Equal(dst[:n], orig) {
		t.Fatalf("decompressed content mismatch")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestZstdDecompressToleratesPadding -v .`
Expected: FAIL with `zstdDecompress: invalid input: magic number mismatch` (the current `DecodeAll` implementation chokes on the zero padding).

- [ ] **Step 3: Rewrite `zstdDecompress` to stream**

In `compress.go`, replace the existing function (`:526-539`):

```go
// zstdDecompress decompresses ZSTD data.
func zstdDecompress(src, dst []byte) (int, error) {
	r, err := zstd.NewReader(bytes.NewReader(src), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, err
	}
	defer r.Close()
	n, err := io.ReadFull(r, dst)
	// The pcluster is zero-padded past the end of the zstd frame; once we
	// have read the expected decompressed size we stop and ignore the rest.
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	return n, nil
}
```

(`bytes` and `io` are already imported in `compress.go`; the `zstd` import stays.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestZstdDecompressToleratesPadding -v .`
Expected: PASS

- [ ] **Step 5: Run the full suite to confirm no regression**

Run: `go test ./...`
Expected: PASS (existing LZ4/deflate/integration tests unaffected).

- [ ] **Step 6: Commit**

```bash
git add compress.go compress_zstd_test.go
git commit -m "fix: stream zstd decompression to tolerate pcluster padding"
```

---

### Task 2: Add the `CompressionZstd` option and a zstd encoder to the Builder

**Files:**
- Modify: `builder_compress.go:1-18`
- Modify: `builder.go:20-30` (Builder struct)

- [ ] **Step 1: Add the `zstdEnc` field to the Builder struct**

In `builder.go`, the struct already has these compression fields (`:28-30`):

```go
	compression     CompressionAlgorithm
	compressEnabled bool
	compressedData  map[*buildInode]*compressedFileData
```

Add one field directly after them:

```go
	compression     CompressionAlgorithm
	compressEnabled bool
	compressedData  map[*buildInode]*compressedFileData
	zstdEnc         *zstd.Encoder
```

- [ ] **Step 2: Add the `CompressionZstd` constant and the encoder import**

In `builder_compress.go`, change the import block (`:3-10`) to add zstd:

```go
import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)
```

Then change the constant block (`:15-18`) to:

```go
const (
	CompressionNone    CompressionAlgorithm = -1
	CompressionAutoLZ4 CompressionAlgorithm = CompressionAlgorithm(ondisk.CompressionLZ4)
	CompressionZstd    CompressionAlgorithm = CompressionAlgorithm(ondisk.CompressionZstd)
)
```

Note: `fmt` is added here because Task 3 uses it for the dispatch default case. `builder.go` already imports `github.com/klauspost/compress/zstd`? It does **not** yet — but `builder_compress.go` now does, and the `zstdEnc` field type resolves through the package import there. Go resolves types per-package, not per-file, so the struct field in `builder.go` compiles as long as any file in the package imports `zstd`. (`builder_compress.go` does.)

- [ ] **Step 3: Add a helper that lazily builds the encoder**

Append to `builder_compress.go`:

```go
// zstdEncoder returns a reusable zstd encoder bound to the builder's block
// size. The window is capped at the block size because every lcluster is
// compressed independently into a single block (h_clusterbits = 0).
func (b *Builder) zstdEncoder() (*zstd.Encoder, error) {
	if b.zstdEnc != nil {
		return b.zstdEnc, nil
	}
	window := b.blockSize
	if window < 1024 { // zstd minimum window size
		window = 1024
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(window),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
	)
	if err != nil {
		return nil, err
	}
	b.zstdEnc = enc
	return enc, nil
}
```

- [ ] **Step 4: Verify it compiles**

Run: `go build ./...`
Expected: builds clean (the new constant, field, helper, and imports resolve; nothing calls them yet).

- [ ] **Step 5: Commit**

```bash
git add builder.go builder_compress.go
git commit -m "feat: add CompressionZstd option and zstd encoder to builder"
```

---

### Task 3: Dispatch compression on the selected algorithm in `tryCompressFile`

**Files:**
- Modify: `builder_compress.go:78-114` (the per-lcluster loop body)

- [ ] **Step 1: Write the failing test**

Add to `builder_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuilderZstdRoundTrip -v .`
Expected: FAIL — `tryCompressFile` still calls LZ4, so the data is written as LZ4 but the inode/map header will (after Task 4) say zstd; right now the content read back will not match (or the file is stored LZ4 and decoded as LZ4, but the algorithm byte is still LZ4 — see Task 4). At this step the failure is the content mismatch / decode error.

- [ ] **Step 3: Replace the LZ4-only compression with a dispatch**

In `builder_compress.go`, the loop body currently reads (`:86-114`):

```go
		// Try LZ4 compression.
		maxOut := lz4.CompressBlockBound(len(chunk))
		compressed := make([]byte, maxOut)
		n, err := lz4.CompressBlock(chunk, compressed, nil)

		if err != nil || n <= 0 || n >= len(chunk) {
```

Replace the three compression lines (down to but not including the `if err != nil ...` test) with:

```go
		// Compress this lcluster using the selected algorithm.
		var compressed []byte
		var n int
		var err error
		switch b.compression {
		case CompressionAutoLZ4:
			maxOut := lz4.CompressBlockBound(len(chunk))
			compressed = make([]byte, maxOut)
			n, err = lz4.CompressBlock(chunk, compressed, nil)
		case CompressionZstd:
			enc, encErr := b.zstdEncoder()
			if encErr != nil {
				return nil, false
			}
			compressed = enc.EncodeAll(chunk, nil)
			n = len(compressed)
		default:
			err = fmt.Errorf("erofs: unsupported compression algorithm %d", b.compression)
		}

		if err != nil || n <= 0 || n >= len(chunk) {
```

The rest of the branch is unchanged: it copies `compressed[:n]` into a `blockSize` pcluster on success, or stores the chunk PLAIN on failure. Because `n >= len(chunk)` (and `len(chunk) <= blockSize`) triggers the PLAIN fallback, an incompressible chunk whose zstd frame is larger than the block is safely stored plain.

- [ ] **Step 4: Run test to verify the data path works**

Run: `go test -run TestBuilderZstdRoundTrip -v .`
Expected: still FAIL **only** if the map-header algorithm byte is wrong (Task 4). If it happens to pass already because Task 4 is done in the same session, fine. If you are doing strict TDD per task, expect this to fail at decode because the map header still says LZ4 (algorithm 0) while the bytes are zstd. Proceed to Task 4.

- [ ] **Step 5: Commit**

```bash
git add builder_compress.go builder_test.go
git commit -m "feat: dispatch lcluster compression on selected algorithm"
```

---

### Task 4: Write the correct algorithm id into the compressed inode map header

**Files:**
- Modify: `builder_compress.go:178-188` (`writeCompressedInode` map header)
- Modify: `builder.go` (add `comprAlgID` helper near `computeComprAlgs`, `:956`)

- [ ] **Step 1: Add the `comprAlgID` helper**

In `builder.go`, directly above `computeComprAlgs` (`:956`), add:

```go
// comprAlgID returns the on-disk compression algorithm id for the builder's
// selected algorithm (e.g. ondisk.CompressionLZ4, ondisk.CompressionZstd).
func (b *Builder) comprAlgID() uint8 {
	return uint8(b.compression)
}
```

This is valid because `CompressionAutoLZ4 == ondisk.CompressionLZ4 (0)` and `CompressionZstd == ondisk.CompressionZstd (3)`. It is only called for compressed inodes, which never exist when compression is `CompressionNone`.

- [ ] **Step 2: Use it in the map header**

In `builder_compress.go`, change the hardcoded line (`:184`):

```go
	mh[6] = ondisk.CompressionLZ4 // h_algorithmtype
```

to:

```go
	mh[6] = b.comprAlgID() // h_algorithmtype (HEAD1)
```

- [ ] **Step 3: Run the zstd round-trip test to verify it passes**

Run: `go test -run TestBuilderZstdRoundTrip -v .`
Expected: PASS — the map header now reports algorithm 3, the reader's `HeadAlgorithm()` returns 3, and `zstdDecompress` (fixed in Task 1) decodes the padded pcluster.

- [ ] **Step 4: Run the LZ4 round-trip test to confirm no regression**

Run: `go test -run TestBuilderCompression -v .`
Expected: PASS — `comprAlgID()` returns 0 for LZ4, identical to the old hardcoded value.

- [ ] **Step 5: Commit**

```bash
git add builder.go builder_compress.go
git commit -m "feat: write selected algorithm id into compressed inode map header"
```

---

### Task 5: Report the correct algorithm in `available_compr_algs`

**Files:**
- Modify: `builder.go:956-962` (`computeComprAlgs`)

- [ ] **Step 1: Write the failing test**

Add to `builder_test.go`:

```go
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
```

Add the imports this test needs to `builder_test.go` if not already present: `"encoding/binary"` and `"github.com/Xe/erofs/internal/ondisk"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuilderZstdAvailComprAlgs -v .`
Expected: FAIL — `computeComprAlgs` returns `1 << CompressionLZ4` (0x0001), but we want `1 << CompressionZstd` (0x0008).

- [ ] **Step 3: Fix `computeComprAlgs`**

In `builder.go`, replace (`:956-962`):

```go
// computeComprAlgs returns the available compression algorithms bitmap.
func (b *Builder) computeComprAlgs() uint16 {
	if len(b.compressedData) == 0 {
		return 0
	}
	return 1 << ondisk.CompressionLZ4
}
```

with:

```go
// computeComprAlgs returns the available compression algorithms bitmap.
func (b *Builder) computeComprAlgs() uint16 {
	if len(b.compressedData) == 0 {
		return 0
	}
	return 1 << b.comprAlgID()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestBuilderZstdAvailComprAlgs -v .`
Expected: PASS

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: PASS (LZ4 still reports 0x0001 via `comprAlgID() == 0`).

- [ ] **Step 6: Commit**

```bash
git add builder.go builder_test.go
git commit -m "feat: report selected algorithm in available_compr_algs"
```

---

### Task 6: Confirm the end-to-end zstd round trip with mixed content

**Files:**
- Modify: `builder_test.go`

This task adds a stronger integration test (compressible + incompressible + small files in one image) to lock in the data path before touching superblock layout in Task 7.

- [ ] **Step 1: Write the test**

Add to `builder_test.go`:

```go
func TestBuilderZstdMixedContent(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(1 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionZstd))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// Highly compressible, spans several lclusters.
	compressible := bytes.Repeat([]byte("aaaaaaaaaaaaaaaa\n"), 2000)
	// Incompressible (a deterministic pseudo-random pattern), forces PLAIN fallback.
	incompressible := make([]byte, 9000)
	for i := range incompressible {
		incompressible[i] = byte((i*2654435761 + 1013904223) >> 13)
	}
	// Small file stays inline/uncompressed.
	small := []byte("hi\n")

	files := map[string][]byte{
		"/comp.txt":   compressible,
		"/incomp.bin": incompressible,
		"/small.txt":  small,
	}
	for name, data := range files {
		b.AddFile(name, &staticFileInfo{
			name: name[1:], mode: 0o644, size: int64(len(data)), mod: epoch,
		}, data)
	}
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	fsys, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name, want := range files {
		got, err := fs.ReadFile(fsys, name[1:])
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: content mismatch (got %d bytes, want %d)", name, len(got), len(want))
		}
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test -run TestBuilderZstdMixedContent -v .`
Expected: PASS — compressible file decodes via zstd, incompressible file round-trips via the PLAIN fallback, small file via inline.

- [ ] **Step 3: Commit**

```bash
git add builder_test.go
git commit -m "test: zstd round trip with mixed compressible content"
```

---

### Task 7: Write the `compr_cfgs` config record and feature flag (kernel compatibility)

> **Validation caveat:** This task targets the Linux kernel / erofs-utils ≥ 1.8 on-disk contract for zstd, which **cannot be validated with the locally installed mkfs.erofs 1.7.1**. The round-trip tests still pass because this project's reader ignores the `compr_cfgs` region. After implementing, flag for out-of-band validation (see Step 7). Do not report kernel compatibility as verified.

**Files:**
- Modify: `internal/ondisk/types.go` (add `ZstdCfgs`)
- Modify: `builder.go` — `computeIncompatFeatures` (`:941`), `assignNIDs` (`:544`), add `comprCfgsSize` + `writeComprCfgs` helpers, call from `Build` (`:264`)

- [ ] **Step 1: Add the `ZstdCfgs` type**

In `internal/ondisk/types.go`, after `LZ4Cfgs` (`:125-130`), add:

```go
// ZstdCfgs is the Zstandard compression configuration (32 bytes), matching
// struct z_erofs_zstd_cfgs in erofs_fs.h. On disk it is preceded by a __le16
// size field in the compression-config area that immediately follows the
// superblock. WindowLog is the ZSTD window log minus ZSTD_WINDOWLOG_ABSOLUTEMIN.
type ZstdCfgs struct {
	Format    uint8
	WindowLog uint8
	Reserved  [30]byte
}
```

- [ ] **Step 2: Write the failing test**

Add to `builder_test.go`:

```go
func TestBuilderZstdComprCfgs(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(256 * 1024)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionZstd))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	data := bytes.Repeat([]byte("zstd cfgs record check\n"), 500)
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	img := buf.Bytes()

	// feature_incompat is a __le32 at superblock offset 80.
	feat := binary.LittleEndian.Uint32(img[1024+80:])
	if feat&ondisk.FeatureIncompatComprCfgs == 0 {
		t.Fatalf("FeatureIncompatComprCfgs not set: feat=0x%08x", feat)
	}

	// compr_cfgs record sits right after the 144-byte superblock: at 1024+144.
	off := 1024 + 144
	size := binary.LittleEndian.Uint16(img[off:])
	if size != 32 {
		t.Fatalf("zstd cfgs size = %d, want 32", size)
	}
	format := img[off+2]
	windowLog := img[off+3]
	if format != 0 {
		t.Fatalf("zstd cfgs format = %d, want 0", format)
	}
	// blkSzBits 12, window log 12, on-disk = 12 - 10 = 2.
	if windowLog != 2 {
		t.Fatalf("zstd cfgs windowlog = %d, want 2", windowLog)
	}

	// The image must still read back correctly with the shifted metadata.
	fsys, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := fs.ReadFile(fsys, "big.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch after cfgs shift")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test -run TestBuilderZstdComprCfgs -v .`
Expected: FAIL — `FeatureIncompatComprCfgs` is not set and no cfgs record is written.

- [ ] **Step 4: Set the feature flag for zstd**

In `builder.go`, `computeIncompatFeatures` (`:941-954`) currently has:

```go
	if len(b.compressedData) > 0 {
		flags |= ondisk.FeatureIncompatZeroPadding
	}
```

Replace that block with:

```go
	if len(b.compressedData) > 0 {
		flags |= ondisk.FeatureIncompatZeroPadding
		if b.compression == CompressionZstd {
			flags |= ondisk.FeatureIncompatComprCfgs
		}
	}
```

LZ4 deliberately keeps using the `lz4_max_distance` superblock union, so we only set `compr_cfgs` for zstd.

- [ ] **Step 5: Add `comprCfgsSize` + `writeComprCfgs`, reserve space in `assignNIDs`, call from `Build`**

Add these helpers to `builder.go` (next to `computeComprAlgs`):

```go
// comprCfgsSize returns the number of bytes occupied by the compression-config
// area that immediately follows the superblock. Only zstd needs it; the record
// is a __le16 size (2 bytes) followed by a 32-byte z_erofs_zstd_cfgs struct.
func (b *Builder) comprCfgsSize() int64 {
	if len(b.compressedData) == 0 || b.compression != CompressionZstd {
		return 0
	}
	return 2 + 32
}

// writeComprCfgs writes the compression-config area for zstd images. The kernel
// reads it from EROFS_SUPER_OFFSET + sizeof(superblock); this builder's
// superblock is 144 bytes, so the area starts at byte 1024+144.
func (b *Builder) writeComprCfgs() error {
	if b.comprCfgsSize() == 0 {
		return nil
	}
	off := int64(ondisk.SuperOffset) + 144
	buf := make([]byte, b.comprCfgsSize())
	binary.LittleEndian.PutUint16(buf[0:], 32) // sizeof(z_erofs_zstd_cfgs)
	buf[2] = 0                                 // format
	buf[3] = b.blkSzBits - 10                  // window log - ZSTD_WINDOWLOG_ABSOLUTEMIN
	// buf[4:34] reserved, already zero.
	if _, err := b.w.WriteAt(buf, off); err != nil {
		return err
	}
	return nil
}
```

In `assignNIDs` (`:564-567`), change the metadata start so it clears the cfgs area:

```go
	// Start after the superblock region.
	// Superblock ends at 1024 + 144 = 1168. Align to 32 = 1184.
	sbEnd := int64(ondisk.SuperOffset) + 144
	slotOff := alignUp(sbEnd, ondisk.ISlotSize)
```

becomes:

```go
	// Start after the superblock and, for zstd, the compression-config area.
	// Superblock ends at 1024 + 144 = 1168; cfgs (if any) follow it.
	sbEnd := int64(ondisk.SuperOffset) + 144 + b.comprCfgsSize()
	slotOff := alignUp(sbEnd, ondisk.ISlotSize)
```

In `Build` (`:264-267`), after `writeSuperblock`, add the cfgs write:

```go
	// Step 8: Write superblock.
	if err := b.writeSuperblock(); err != nil {
		return err
	}

	// Step 8b: Write the compression-config area (zstd only).
	if err := b.writeComprCfgs(); err != nil {
		return fmt.Errorf("erofs: writing compr cfgs: %w", err)
	}
```

(`fmt` is already imported in `builder.go`.)

- [ ] **Step 6: Run test to verify it passes**

Run: `go test -run TestBuilderZstdComprCfgs -v .`
Expected: PASS — feature flag set, cfgs record at 1168, metadata shifted, content still reads back.

- [ ] **Step 7: Run the full suite, then flag for out-of-band validation**

Run: `go test ./...`
Expected: PASS.

Then attempt reference validation **if and only if** erofs-utils ≥ 1.8 is available:

```bash
# Build a real on-disk image via the CLI (after Task 8) or a small harness,
# then validate with newer tooling. mkfs.erofs 1.7.1 (local) CANNOT do this.
fsck.erofs --version   # need >= 1.8 for zstd
fsck.erofs /path/to/zstd-image.erofs
dump.erofs -s /path/to/zstd-image.erofs   # inspect available_compr_algs / cfgs
```

If only 1.7.1 is available, record in the commit/PR description that kernel-side validation is **pending** and was not performed. Do not claim it passed.

- [ ] **Step 8: Commit**

```bash
git add internal/ondisk/types.go builder.go builder_test.go
git commit -m "feat: write zstd compr_cfgs record and incompat feature flag"
```

---

### Task 8: Expose compression choice in the `mkfs.erofs` CLI

**Files:**
- Modify: `cmd/mkfs.erofs/main.go:13-18` (flags), `:42-47` (builder construction)

- [ ] **Step 1: Add a `--compression` flag**

In `cmd/mkfs.erofs/main.go`, the flag block (`:13-18`) currently ends with `out`. Add after it:

```go
	compression = pflag.StringP("compression", "z", "lz4", "compression algorithm: none, lz4, or zstd")
```

- [ ] **Step 2: Map the flag to a `BuildOption`**

Replace the builder construction (`:42-47`):

```go
	b := erofs.NewBuilder(
		fout,
		erofs.WithBlockSize(*blockSize),
		erofs.WithEpoch(*epoch),
		erofs.WithCompression(erofs.CompressionAutoLZ4),
	)
```

with:

```go
	opts := []erofs.BuildOption{
		erofs.WithBlockSize(*blockSize),
		erofs.WithEpoch(*epoch),
	}
	switch *compression {
	case "none":
		// no compression option
	case "lz4":
		opts = append(opts, erofs.WithCompression(erofs.CompressionAutoLZ4))
	case "zstd":
		opts = append(opts, erofs.WithCompression(erofs.CompressionZstd))
	default:
		fmt.Fprintf(os.Stderr, "unknown compression %q (want none, lz4, or zstd)\n", *compression)
		os.Exit(2)
	}

	b := erofs.NewBuilder(fout, opts...)
```

(`fmt` and `os` are already imported in `main.go`.)

- [ ] **Step 3: Verify it builds and runs**

Run:
```bash
go build ./...
go run ./cmd/mkfs.erofs --help 2>&1 | grep -A1 compression
```
Expected: builds clean; help lists the `-z, --compression` flag with default `lz4`.

- [ ] **Step 4: Smoke-test a real zstd image and read it back via the inspector**

Run:
```bash
mkdir -p /tmp/erofs-zstd-src && printf 'hello zstd %.0s' {1..2000} > /tmp/erofs-zstd-src/big.txt
go run ./cmd/mkfs.erofs -i /tmp/erofs-zstd-src -o /tmp/zstd.img -z zstd
go run ./cmd/erofs-inspect /tmp/zstd.img | grep -i "AvailComprAlgs\|FeatureIncompat"
```
Expected: `AvailComprAlgs: 0x0008` and `FeatureIncompat` includes the `compr_cfgs` bit (0x2). (Note: this validates *our* encoder/inspector pair, not the kernel.)

- [ ] **Step 5: Commit**

```bash
git add cmd/mkfs.erofs/main.go
git commit -m "feat(mkfs): add --compression flag with zstd support"
```

---

### Task 9: Update project documentation

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Correct the compression status note**

`CLAUDE.md` currently states the project "Supports LZ4, LZMA, DEFLATE, and Zstandard compression" without distinguishing reader vs. builder. Update the Project section to be precise. Change:

```markdown
Supports LZ4, LZMA, DEFLATE, and Zstandard compression. Output is bytewise compatible with the Linux kernel EROFS driver and mkfs.erofs.
```

to:

```markdown
Reader supports LZ4, LZMA, DEFLATE, and Zstandard decompression. Builder
emits LZ4 or Zstandard. Output is intended to be bytewise compatible with the
Linux kernel EROFS driver and mkfs.erofs; zstd compr_cfgs output has not yet
been validated against erofs-utils >= 1.8 (1.7.1 lacks zstd).
```

- [ ] **Step 2: Run the full suite one last time**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 3: Format imports**

Run: `go tool goimports -w .`
Expected: no diff, or only import reordering.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: clarify zstd builder support and validation status"
```

---

## Self-Review

**Spec coverage** (against "implement zstd in the builder/reader"):
- Reader zstd actually works on real (padded) pclusters → Task 1.
- Builder emits zstd-compressed data → Tasks 2–3.
- Builder writes correct algorithm id in the map header → Task 4.
- Builder advertises zstd in `available_compr_algs` → Task 5.
- End-to-end correctness incl. PLAIN fallback → Task 6.
- Kernel/mkfs on-disk contract (`compr_cfgs` + feature flag + metadata shift) → Task 7.
- User-facing entry point → Task 8 (CLI).
- Accurate docs → Task 9.

**Placeholder scan:** No TBD/TODO; every code step has concrete code and exact commands with expected output. The one value that depends on the external spec (zstd on-disk `windowlog`) is given concretely as `blkSzBits - 10` with the constant named (`ZSTD_WINDOWLOG_ABSOLUTEMIN`) and is explicitly flagged as unvalidated against erofs-utils ≥ 1.8 in Task 7.

**Type consistency:** `CompressionZstd` (builder_compress.go) used consistently across Tasks 2/4/7/8. `comprAlgID()` defined Task 4, reused Task 5. `comprCfgsSize()` defined Task 7, used in both `assignNIDs` and `writeComprCfgs`. `zstdEncoder()`/`zstdEnc` defined Task 2, used Task 3. `ZstdCfgs` struct (Task 7) matches the bytes written in `writeComprCfgs` (format at +2, windowlog at +3, after the +0 `__le16` size).

**Known risk carried forward:** The `compr_cfgs` offset assumes the kernel's `sizeof(struct erofs_super_block)` equals this project's 144-byte layout. If a target kernel uses a different superblock size, the cfgs offset will not match. This is inherent to the existing superblock choice and is called out in Task 7's caveat.
