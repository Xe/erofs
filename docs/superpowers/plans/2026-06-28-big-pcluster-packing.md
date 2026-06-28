# Big-pcluster Packing Implementation Plan (issue #5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make compressed EROFS images actually smaller by packing each pcluster's compressed bytes into `ceil(compressed_len / block_size)` physical blocks (a "big pcluster" spanning several logical lclusters) instead of padding every lcluster to its own full block; and fix the `BlocksLo` superblock undercount along the way.

**Architecture:** The reader already has a big-pcluster code path but it has never been exercised and contains real bugs (verified against `mkfs.erofs`-produced images committed under `testdata/`). We fix the reader first, then change the builder so `tryCompressFile` groups lclusters into pclusters, compresses each group as one unit, packs the compressed bytes densely, and emits one `HEAD1` + N `NONHEAD` index entries with the `D0_CBLKCNT` compressed-block count, matching the exact encoding `mkfs.erofs` writes. `BlocksLo` is fixed as a small standalone first step.

**Tech Stack:** Go, `github.com/pierrec/lz4/v4`, `github.com/klauspost/compress/zstd`, `internal/ondisk`.

---

## Background facts (verified against the codebase and real tooling before writing this plan)

These were confirmed empirically while writing the plan — do not re-derive them, but do re-run the verification commands if a step behaves unexpectedly.

### On-disk encoding ground truth (from `mkfs.erofs` 1.7.1)

`mkfs.erofs -zlz4 -C65536 -b4096 -E legacy-compress` produces a FULL (legacy, layout 1) compressed index. For a 135000-byte file (33 lclusters at 4096) that compresses to a single physical block (C=1), the raw `z_erofs_lcluster_index` entries are:

```
maphdr: advise=0x0002 (BIG_PCLUSTER_1), algorithmtype=0x00 (LZ4 HEAD1), clusterbits=0x00
lcn 0  HEAD1   clusterofs=0      blkaddr=1
lcn 1  NONHEAD delta0=0x0801     delta1=31      (0x0801 = D0_CBLKCNT(0x800) | cblkcnt(1))
lcn 2  NONHEAD delta0=2          delta1=30
lcn 3  NONHEAD delta0=3          delta1=29
...
lcn 31 NONHEAD delta0=31         delta1=1
lcn 32 PLAIN   clusterofs=3928   blkaddr=0      (partial-tail boundary marker; 135000 % 4096 = 3928)
```

Decoded rules (h_clusterbits = 0, so lcluster size == block size):
- **HEAD** lcluster: `clusterofs` = byte offset within the lcluster where the extent's decompressed output begins (0 for a fresh, aligned pcluster); `di_u.blkaddr` = physical block address where the pcluster's compressed blocks start.
- **NONHEAD** lcluster `j` (relative to head `h`): `delta[0]` = backward distance to the head (`j - h`), **except the first NONHEAD** (`j = h+1`) where `delta[0]` carries the compressed-block count tagged with `Z_EROFS_LI_D0_CBLKCNT` (bit 11, `0x800`); the kernel resets that to distance 1 on read. `delta[1]` = `lastLcn - j`, where `lastLcn` is the last lcluster of the pcluster (the marker lcn if there is a partial tail).
- **Partial-tail marker**: when the file ends mid-lcluster, the final lcluster is emitted as a `PLAIN` entry with `clusterofs = size % block_size` and `blkaddr = 0` (it owns **no** data block; its bytes come from the head pcluster's decompression). The extent's decompressed length is `markerLcn * lclusterSize + marker.clusterofs`.
- `EROFS_FEATURE_INCOMPAT_BIG_PCLUSTER` and `EROFS_FEATURE_INCOMPAT_COMPR_CFGS` are the **same bit** `0x00000002` (already both named in `internal/ondisk/constants.go:33-34`).
- `Z_EROFS_ADVISE_BIG_PCLUSTER_1` is `0x0002` (`ondisk.AdviseBigPCluster1`).

### Reader bugs (all confirmed by reading the committed fixtures with the current reader)

The current `loadPClusterFull` (`compress.go:153-298`) fails on real big-pcluster images. Four distinct fixes are needed, all verified to produce correct reads of both committed fixtures (including random access):

1. **Extent-end miscomputed** (`compress.go:233`): `extentEnd = scanLcn * cf.lclustSz` ignores the boundary entry's `clusterofs`. Must be `scanLcn*cf.lclustSz + int64(scanEntry.ClusterOfs)`. Without this, the 135000-byte file decompresses only 131072 bytes → `lz4: invalid source or destination buffer too short`.
2. **Lookback ignores `D0_CBLKCNT`** (`compress.go:171-184`): the first NONHEAD's `delta[0]` is `cblkcnt | 0x800`; taken literally the lookback walks negative (`NONHEAD chain went negative at lcn=1`). Must mask the bit and treat distance as 1.
3. **`D0_CBLKCNT` read from the wrong field** (`compress.go:212-216`): checks `nextEntry.Advise & LID0CBlkCnt` but the flag lives in `delta[0]` (`di_u`), not `di_advise`; and the two-line mask is dead/confused. Must check `nextEntry.Delta0() & LID0CBlkCnt` and extract `nextEntry.Delta0() &^ LID0CBlkCnt`. (Did not bite the C=1 fixture because the default of 1 happened to be correct; bites the C=11 fixture.)
4. **Random access into the partial-tail marker hangs**: reading an offset that lands directly on the `PLAIN` marker lcluster yields a degenerate extent (`decompSize <= 0`, cache never advances → infinite loop). A `PLAIN` lcluster with non-zero `clusterofs` in a non-interlaced image is a tail marker whose bytes belong to the preceding pcluster; resolve the offset there.

`lz4Decompress` (`compress.go:509-524`) needs **no** change — its trailing-zero trim is fine; the earlier "buffer too short" was solely the `decompSize` bug (fix #1).

### Already-landed prerequisite fixes (do NOT redo)

While preparing this plan, three correctness bugs surfaced via `fsck.erofs` and were fixed directly in the working tree (with `TestBuilderLargeDirectory` covering them). The big-pcluster work builds on top of these — do not re-implement them, and note that Task 1 below is therefore already done:

- **`BlocksLo` undercount (issue #5 secondary bug) — FIXED comprehensively.** `layoutDataBlocks` now records `b.endDataBlk = currentBlk` (the first block past all out-of-line data, covering FLAT_PLAIN, FLAT_INLINE preceding blocks, *and* compressed pclusters, since `layoutCompressedBlocks` advances `currentBlk`). `writeSuperblock` uses `max(metadata-region blocks, endDataBlk)`. This supersedes the compressed-only fix originally drafted as Task 1.
- **Inline tail crossing a block boundary — FIXED.** `assignNIDs` now pads to the next block when an inode's header (and, for FLAT_INLINE, its inline tail) would straddle a block boundary.
- **`nlink` written into the wrong extended-inode field — FIXED.** Per `erofs_fs.h`, the extended (64-byte) inode has `i_nb` at offset 6 (`nlink`/`blocks_hi`/`startblk_hi`) *and* a separate `i_nlink` at offset 44; the offset-6 union is only `nlink` for **compact** inodes. The builder wrote `nlink` into offset 6, corrupting the high block-address bits under the 48-bit reading fsck/kernel apply to extended inodes. Builder now writes `NB = 0` (high bits) in all three extended-inode writers and keeps `nlink` in `i_nlink`; the reader's `parseUnion` is version-aware (extended → offset 6 is `startblk_hi`, `nlink` from `i_nlink`; compact → unchanged).

### Builder facts

- `tryCompressFile` (`builder_compress.go:60-150`) compresses each lcluster independently and pads each to a full block (`builder_compress.go:118,128`); `layoutCompressedBlocks` (`:237-243`) gives every entry its own block. This is the root cause of "compressed images aren't smaller."
- `compressedFileData` (`builder_compress.go:152-157`) currently has `pclusters [][]byte` (one block per entry). This is replaced by `blocks [][]byte` + a parallel `blockOf []int` (local block index per index entry, or `-1` for entries that own no block: NONHEADs and tail markers).
- The build pipeline (`builder.go:209-284`) runs: `computeLayouts` → `tryCompressInodes` (`:491-505`, sets `dataLayout=InodeCompressedFull`, `metaSize=computeCompressedMetaSize(len(indexEntries))`) → `assignNIDs` → `layoutDataBlocks` (`:631-681`, calls `layoutCompressedBlocks`) → `writeMetadata` (`writeCompressedInode`) → `writeDataBlocks` (`:771-811`, calls `writeCompressedBlocks(cdata)` at `:779`) → `writeComprCfgs` → `writeSuperblock` (`BlocksLo`/`maxOff` at `:824-838`).
- `computeCompressedMetaSize(numLclusters)` (`builder_compress.go:161-164`) returns `64+8+8+N*8` and is called with `len(cdata.indexEntries)` — index size tracks **entries**, not blocks, so it is unaffected by dense packing.
- The zstd encoder window (`builder_compress.go:259-278`) and `writeComprCfgs` windowlog (`builder.go:992-1018`, currently `blkSzBits-10`) are tied to block size; with big pclusters they must track the pcluster size.
- `WithBlockSize(bits)` (`builder.go:69-75`) and the `Builder` struct (`builder.go:20-37`) are where the new `WithPClusterSize`/`pclusterBits` field hangs.

### Validation tooling — READ THIS

- `mkfs.erofs`, `fsck.erofs`, `dump.erofs` are installed at **version 1.7.1**, which supports **LZ4** big pclusters (`-C`, `-E legacy-compress`) but has **no zstd** support (zstd landed in erofs-utils 1.8).
- Therefore builder-produced **LZ4** big-pcluster images **can and must** be validated locally with `fsck.erofs` (Task 7). Builder-produced **zstd** images **cannot** be validated locally — flag that for out-of-band validation with erofs-utils ≥ 1.8, exactly as the prior zstd plan did. Do not claim kernel/zstd compatibility is verified.
- The reader fixes are validated against the committed real LZ4 fixtures, which is fully sufficient.

### Committed fixtures (already in the tree)

`testdata/bigpcluster-lz4-c1.img` (+`.txt`) and `testdata/bigpcluster-lz4-c11.img` (+`.bin`) are real `mkfs.erofs` FULL big-pcluster images (C=1 and C=11), documented in `testdata/README.md`, both passing `fsck.erofs`. Task 2 uses them as the reader regression corpus.

---

## File Structure

- `builder.go` — **Modify** `BlocksLo`/`maxOff` loop (Task 1); add `pclusterBits` struct field + `WithPClusterSize` + `pclusterBitsEff`/`pclusterSizeEff`/`pclusterLclustersEff` helpers (Task 3); update `computeIncompatFeatures` (big-pcluster bit) and `writeComprCfgs` (windowlog) and the `writeCompressedBlocks` call site (Task 4/6).
- `compress.go` — **Modify** `loadPClusterFull` only: four reader fixes (Task 2).
- `builder_compress.go` — **Modify** `compressedFileData`, `tryCompressFile` (rewrite to group + pack + encode), `layoutCompressedBlocks`, `writeCompressedBlocks`, `writeCompressedInode` (advise bits), `zstdEncoder` (window); add `compressGroup` helper (Task 4/6).
- `compress.go` / `builder_test.go` — **Modify** tests that assume per-lcluster layout (the two zstd cfgs windowlog tests, the mixed-content structural assertion) (Task 4/6).
- `compress_bigpcluster_test.go` — **Create** reader fixture tests (Task 2).
- `builder_bigpcluster_test.go` — **Create** builder encoding + size-regression + round-trip + fsck tests (Tasks 5, 7).
- `CLAUDE.md`, `CHANGELOG.md` — **Modify** to describe big-pcluster support and remaining validation caveats (Task 8).

---

### Task 1: Fix the `BlocksLo` undercount — ALREADY DONE

> **Status: already implemented** (see "Already-landed prerequisite fixes" above). `layoutDataBlocks` records `b.endDataBlk` and `writeSuperblock` uses it, which covers compressed pclusters too. The steps below are retained only as a record of intent and the regression assertion; the executor should **skip implementation** and instead confirm the existing behavior with the test in Step 1 (it should already pass). Do not re-add the compressed-only `maxOff` branch — it is redundant with `endDataBlk` and would double-count.

**Files:**
- Already modified: `builder.go` (`layoutDataBlocks` sets `endDataBlk`; `writeSuperblock` uses it)
- Test: add `TestBuilderBlocksLoCountsCompressed` to `builder_bigpcluster_test.go` (the big-pcluster suite) if not already covered by `TestBuilderLargeDirectory`

- [ ] **Step 1: Write the failing test**

Create `builder_bigpcluster_test.go`:

```go
package erofs

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"testing"
	"time"
)

// blocksLo reads the superblock BlocksLo (__le32 at superblock offset 12).
func blocksLo(img []byte) uint32 {
	return binary.LittleEndian.Uint32(img[1024+12:])
}

func TestBuilderBlocksLoCountsCompressed(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(4 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	data := bytes.Repeat([]byte("compressible payload that spans many blocks\n"), 50000)
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	img := buf.Bytes()

	got := int64(blocksLo(img)) * 4096
	// BlocksLo*blockSize must cover the whole image (within one block of slack).
	if got < int64(len(img))-4096 || got > int64(len(img)) {
		t.Fatalf("BlocksLo*blockSize = %d, want ~image size %d", got, len(img))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestBuilderBlocksLoCountsCompressed -v .`
Expected: FAIL — `BlocksLo*blockSize` is a tiny value (the bug reports ~2 blocks for a multi-MB compressed image).

- [ ] **Step 3: Extend the `maxOff` loop to include compressed extents**

In `builder.go`, replace the loop body at `:826-836`:

```go
	for _, ino := range b.inodes {
		if ino.dataLayout == ondisk.InodeFlatPlain && len(ino.data) > 0 {
			end := int64(ino.startBlk)*int64(b.blockSize) + int64(len(ino.data))
			if end > maxOff {
				maxOff = end
			}
		}
		end := ino.metaOff + int64(ino.metaSize)
		if end > maxOff {
			maxOff = end
		}
	}
```

with:

```go
	for _, ino := range b.inodes {
		if ino.dataLayout == ondisk.InodeFlatPlain && len(ino.data) > 0 {
			end := int64(ino.startBlk)*int64(b.blockSize) + int64(len(ino.data))
			if end > maxOff {
				maxOff = end
			}
		}
		// Compressed inodes occupy len(cdata.pclusters) physical blocks
		// starting at ino.startBlk; FlatPlain accounting above skips them.
		if cdata, ok := b.compressedData[ino]; ok {
			end := (int64(ino.startBlk) + int64(len(cdata.pclusters))) * int64(b.blockSize)
			if end > maxOff {
				maxOff = end
			}
		}
		end := ino.metaOff + int64(ino.metaSize)
		if end > maxOff {
			maxOff = end
		}
	}
```

> Note for Task 4: `cdata.pclusters` is renamed to `cdata.blocks` there; this expression becomes `len(cdata.blocks)`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestBuilderBlocksLoCountsCompressed -v .`
Expected: PASS.

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
go tool goimports -w .
git add builder.go builder_bigpcluster_test.go
git commit -m "fix(builder): count compressed data blocks in superblock BlocksLo"
```

---

### Task 2: Fix the reader's big-pcluster decode path

Make `loadPClusterFull` correctly read real big-pcluster images. Verified against the committed `fsck.erofs`-clean fixtures.

**Files:**
- Modify: `compress.go:153-298` (`loadPClusterFull`)
- Test: `compress_bigpcluster_test.go` (create)

- [ ] **Step 1: Write the failing fixture round-trip test**

Create `compress_bigpcluster_test.go`:

```go
package erofs

import (
	"bytes"
	"io/fs"
	"os"
	"testing"
)

func TestReaderBigPClusterFixtures(t *testing.T) {
	cases := []struct{ img, name, src string }{
		{"testdata/bigpcluster-lz4-c1.img", "big.txt", "testdata/bigpcluster-lz4-c1.txt"},
		{"testdata/bigpcluster-lz4-c11.img", "semi.bin", "testdata/bigpcluster-lz4-c11.bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(tc.img)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			fsys, err := Open(f)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := fs.ReadFile(fsys, tc.name)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			want, err := os.ReadFile(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: content mismatch (got %d bytes, want %d)", tc.name, len(got), len(want))
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestReaderBigPClusterFixtures -v .`
Expected: FAIL — `c1` fails with `lz4: invalid source or destination buffer too short`; `c11` fails similarly.

- [ ] **Step 3: Apply reader fix #4 — redirect tail-marker lclusters**

In `compress.go`, immediately after the initial entry read in `loadPClusterFull` (after `:164-167`, the `entry, err := cf.readLClusterEntry(indexStart, lcn)` block), insert:

```go
	// A PLAIN lcluster carrying a non-zero clusterofs (in a non-interlaced
	// image) is a big-pcluster tail boundary marker: its bytes belong to the
	// preceding pcluster's extent. Resolve the offset there instead.
	if entry.Type() == ondisk.LClusterTypePlain && entry.ClusterOfs != 0 &&
		cf.mapHeader.Advise&ondisk.AdviseInterlacedPCluster == 0 && lcn > 0 {
		lcn--
		entry, err = cf.readLClusterEntry(indexStart, lcn)
		if err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Apply reader fix #2 — mask `D0_CBLKCNT` during lookback**

In `compress.go`, replace the lookback loop body (`:171-184`):

```go
	for entry.Type() == ondisk.LClusterTypeNonHead {
		delta := int64(entry.Delta0())
		if delta == 0 {
			return fmt.Errorf("erofs: zero delta in NONHEAD chain at lcn=%d", headLcn)
		}
		headLcn -= delta
```

with:

```go
	for entry.Type() == ondisk.LClusterTypeNonHead {
		delta := int64(entry.Delta0())
		// On the first NONHEAD of a big pcluster, delta[0] carries the
		// compressed block count tagged with D0_CBLKCNT; the real backward
		// distance to the head is 1.
		if delta&ondisk.LID0CBlkCnt != 0 {
			delta = 1
		}
		if delta == 0 {
			return fmt.Errorf("erofs: zero delta in NONHEAD chain at lcn=%d", headLcn)
		}
		headLcn -= delta
```

- [ ] **Step 5: Apply reader fix #3 — read `D0_CBLKCNT` from `delta[0]`**

In `compress.go`, replace the cblkcnt block (`:209-218`):

```go
	if nextLcn < totalLclusters {
		nextEntry, err := cf.readLClusterEntry(indexStart, nextLcn)
		if err == nil && nextEntry.Type() == ondisk.LClusterTypeNonHead {
			if nextEntry.Advise&ondisk.LID0CBlkCnt != 0 {
				pclusterBlocks = int64(nextEntry.Delta0() & ^uint16(ondisk.LID0CBlkCnt>>0))
				// D0_CBLKCNT is bit 11, so mask it off from delta0.
				pclusterBlocks = int64(nextEntry.Delta0()) & ((1 << 11) - 1)
			}
		}
	}
```

with:

```go
	if nextLcn < totalLclusters {
		nextEntry, err := cf.readLClusterEntry(indexStart, nextLcn)
		if err == nil && nextEntry.Type() == ondisk.LClusterTypeNonHead &&
			nextEntry.Delta0()&ondisk.LID0CBlkCnt != 0 {
			pclusterBlocks = int64(nextEntry.Delta0() &^ uint16(ondisk.LID0CBlkCnt))
		}
	}
	if pclusterBlocks < 1 {
		pclusterBlocks = 1
	}
```

- [ ] **Step 6: Apply reader fix #1 — include `clusterofs` in extent end**

In `compress.go`, replace the forward-scan assignment (`:233`):

```go
			extentEnd = scanLcn * cf.lclustSz
```

with:

```go
			extentEnd = scanLcn*cf.lclustSz + int64(scanEntry.ClusterOfs)
```

- [ ] **Step 7: Run the fixture test to verify it passes**

Run: `go test -run TestReaderBigPClusterFixtures -v .`
Expected: PASS for both `big.txt` (C=1) and `semi.bin` (C=11).

- [ ] **Step 8: Add the random-access regression test**

Append to `compress_bigpcluster_test.go`:

```go
import "io" // add to the existing import block

func TestReaderBigPClusterRandomAccess(t *testing.T) {
	f, err := os.Open("testdata/bigpcluster-lz4-c1.img")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fsys, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/bigpcluster-lz4-c1.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 5000, 100000, 131072, 134000, 134999} {
		file, err := fsys.Open("big.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.(io.Seeker).Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 200)
		n, _ := io.ReadFull(file, buf) // short read near EOF is fine
		file.Close()
		if n == 0 || !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
			t.Fatalf("offset %d: mismatch (n=%d)", off, n)
		}
	}
}
```

- [ ] **Step 9: Run the random-access test and the full suite**

Run: `go test -run TestReaderBigPCluster -v .` then `go test ./...`
Expected: all PASS. (On the unfixed reader, offset 131072 would have hung — this confirms fix #4.)

- [ ] **Step 10: Commit**

```bash
go tool goimports -w .
git add compress.go compress_bigpcluster_test.go
git commit -m "fix(reader): correctly decode big-pcluster compressed images"
```

---

### Task 3: Add `WithPClusterSize` and pcluster-sizing helpers

Additive, no behavior change yet (helpers unused until Task 4). Keeps the tree green.

**Files:**
- Modify: `builder.go:20-37` (struct), `:69-75` (near `WithBlockSize`)

- [ ] **Step 1: Add the struct field**

In `builder.go`, add to the `Builder` struct (after `blkSzBits uint8` at `:23`):

```go
	pclusterBits    uint8 // log2 of pcluster size; 0 = auto (blkSzBits+4)
```

- [ ] **Step 2: Add the option and helpers**

In `builder.go`, after `WithBlockSize` (`:75`), add:

```go
// WithPClusterSize sets the pcluster (compression unit) size as a power-of-two
// bit count, e.g. 16 for 64 KiB. One pcluster spans pclusterSize/blockSize
// logical lclusters and is compressed as a single unit, so its compressed
// output can occupy fewer physical blocks than the logical span. Defaults to
// blockSize*16 (capped at 1 MiB, floored at 2 lclusters).
func WithPClusterSize(bits uint8) BuildOption {
	return func(b *Builder) {
		b.pclusterBits = bits
	}
}

// pclusterBitsEff returns the effective pcluster size in bits, applying the
// auto default and clamping. A pcluster must span at least two lclusters for a
// big pcluster to be possible, and is capped at 1 MiB (Z_EROFS_PCLUSTER_MAX).
func (b *Builder) pclusterBitsEff() uint8 {
	pb := b.pclusterBits
	if pb == 0 {
		pb = b.blkSzBits + 4 // 16 lclusters
	}
	if pb < b.blkSzBits+1 {
		pb = b.blkSzBits + 1
	}
	if pb > 20 {
		pb = 20
	}
	return pb
}

func (b *Builder) pclusterSizeEff() int { return 1 << b.pclusterBitsEff() }

func (b *Builder) pclusterLclustersEff() int { return b.pclusterSizeEff() / b.blockSize }
```

- [ ] **Step 3: Verify it compiles and the suite is green**

Run: `go build ./... && go test ./...`
Expected: all PASS (helpers compile; Go does not error on unused methods).

- [ ] **Step 4: Commit**

```bash
git add builder.go
git commit -m "feat(builder): add WithPClusterSize option and pcluster sizing helpers"
```

---

### Task 4: Pack lclusters into big pclusters in the builder

The core change. Replace per-lcluster padding with grouped compression, dense block packing, and the exact `HEAD1`/`NONHEAD`/tail-marker index encoding `mkfs.erofs` uses. This task must land atomically (struct, encoder, layout, writer, feature/advise bits, and the two affected cfgs tests all change together) to keep the tree green.

**Files:**
- Modify: `builder_compress.go` (`compressedFileData`, `tryCompressFile`, `compressGroup` [new], `layoutCompressedBlocks`, `writeCompressedBlocks`, `writeCompressedInode`, `zstdEncoder`)
- Modify: `builder.go` (`writeCompressedBlocks` call site `:779`, `computeIncompatFeatures` `:952-967`, `writeComprCfgs` `:992-1018`, `BlocksLo` line from Task 1)
- Modify: `builder_test.go` (`TestBuilderZstdComprCfgs`, `TestBuilderZstdComprCfgsBlockSize`, `TestBuilderZstdMixedContent`)

- [ ] **Step 1: Replace `compressedFileData` and add `compressGroup`**

In `builder_compress.go`, replace the struct (`:152-157`):

```go
// compressedFileData holds the compression results for a single file.
type compressedFileData struct {
	pclusters    [][]byte
	indexEntries []ondisk.LClusterIndex
	lclusterSize int
}
```

with:

```go
// compressedFileData holds the compression results for a single file.
//
// blocks is the dense, ordered list of physical blocks (each blockSize bytes)
// the inode occupies. indexEntries has one entry per logical lcluster. blockOf
// maps each index entry to its local block index within blocks, or -1 for
// entries that own no block (NONHEAD continuations and partial-tail markers).
type compressedFileData struct {
	blocks       [][]byte
	indexEntries []ondisk.LClusterIndex
	blockOf      []int
	lclusterSize int
}

// compressGroup compresses one pcluster's worth of bytes as a single unit.
// Returns (compressed, true) when compression produced output; (nil, false)
// when the data is incompressible (LZ4 reports no gain).
func (b *Builder) compressGroup(group []byte) ([]byte, bool) {
	switch b.compression {
	case CompressionAutoLZ4:
		dst := make([]byte, lz4.CompressBlockBound(len(group)))
		n, err := lz4.CompressBlock(group, dst, nil)
		if err != nil || n <= 0 {
			return nil, false
		}
		return dst[:n], true
	case CompressionZstd:
		enc, err := b.zstdEncoder()
		if err != nil {
			return nil, false
		}
		return enc.EncodeAll(group, nil), true
	default:
		return nil, false
	}
}
```

- [ ] **Step 2: Rewrite `tryCompressFile`**

In `builder_compress.go`, replace the body of `tryCompressFile` from the data-loop onward (`:81-150`, everything after the `switch b.compression { ... }` guard at `:74-79`) with the grouped implementation. The final function reads:

```go
func (b *Builder) tryCompressFile(ino *buildInode) (*compressedFileData, bool) {
	if !b.compressEnabled || b.compression == CompressionNone {
		return nil, false
	}
	if isIncompressible(ino.path, ino.size) {
		return nil, false
	}
	// Don't compress very small files -- inline is better.
	if ino.size <= int64(b.blockSize) {
		return nil, false
	}
	switch b.compression {
	case CompressionAutoLZ4, CompressionZstd:
		// supported
	default:
		return nil, false
	}

	data := ino.data
	bs := b.blockSize
	size := len(data)
	K := b.pclusterLclustersEff() // logical lclusters per pcluster (>= 2)
	tail := size % bs             // partial-tail bytes (0 if block-aligned)
	totalLclusters := (size + bs - 1) / bs

	var (
		blocks  [][]byte
		entries []ondisk.LClusterIndex
		blockOf []int
		anyBig  bool
	)

	lcn := 0
	for lcn < totalLclusters {
		groupEnd := lcn + K
		if groupEnd >= totalLclusters {
			groupEnd = totalLclusters
		} else if groupEnd == totalLclusters-1 && tail != 0 {
			// Absorb a lone trailing partial lcluster into this group so it
			// never forms its own 1-lcluster group.
			groupEnd = totalLclusters
		}
		spanLcl := groupEnd - lcn
		g0 := lcn * bs
		g1 := groupEnd * bs
		if g1 > size {
			g1 = size // final group includes the partial tail
		}
		group := data[g0:g1]

		comp, ok := b.compressGroup(group)
		cBlocks := 0
		if ok {
			cBlocks = (len(comp) + bs - 1) / bs
		}

		if ok && cBlocks > 0 && cBlocks < spanLcl {
			// Big pcluster: dense compressed blocks + HEAD/NONHEAD index.
			headBlock := len(blocks)
			packed := make([]byte, cBlocks*bs)
			copy(packed, comp)
			for i := 0; i < cBlocks; i++ {
				blocks = append(blocks, packed[i*bs:(i+1)*bs])
			}
			lastLcn := groupEnd - 1
			tailMarker := groupEnd == totalLclusters && tail != 0
			for j := 0; j < spanLcl; j++ {
				cur := lcn + j
				switch {
				case j == 0:
					entries = append(entries, ondisk.LClusterIndex{
						Advise: ondisk.LClusterTypeHead1, ClusterOfs: 0,
						Union: uint32(headBlock),
					})
					blockOf = append(blockOf, headBlock)
				case tailMarker && cur == lastLcn:
					// Partial-tail boundary marker (owns no data block).
					entries = append(entries, ondisk.LClusterIndex{
						Advise: ondisk.LClusterTypePlain, ClusterOfs: uint16(tail),
						Union: 0,
					})
					blockOf = append(blockOf, -1)
				default:
					var d0 uint16
					if j == 1 {
						d0 = uint16(cBlocks) | uint16(ondisk.LID0CBlkCnt)
					} else {
						d0 = uint16(j)
					}
					d1 := uint16(lastLcn - cur)
					entries = append(entries, ondisk.LClusterIndex{
						Advise: ondisk.LClusterTypeNonHead, ClusterOfs: 0,
						Union: uint32(d0) | uint32(d1)<<16,
					})
					blockOf = append(blockOf, -1)
				}
			}
			anyBig = true
		} else {
			// Incompressible group: store each lcluster as its own PLAIN block.
			for j := 0; j < spanLcl; j++ {
				cur := lcn + j
				s := cur * bs
				e := s + bs
				if e > size {
					e = size
				}
				blk := make([]byte, bs)
				copy(blk, data[s:e])
				lb := len(blocks)
				blocks = append(blocks, blk)
				entries = append(entries, ondisk.LClusterIndex{
					Advise: ondisk.LClusterTypePlain, ClusterOfs: 0,
					Union: uint32(lb),
				})
				blockOf = append(blockOf, lb)
			}
		}
		lcn = groupEnd
	}

	if !anyBig {
		// Nothing benefited from a big pcluster -- store flat instead.
		return nil, false
	}

	return &compressedFileData{
		blocks:       blocks,
		indexEntries: entries,
		blockOf:      blockOf,
		lclusterSize: bs,
	}, true
}
```

- [ ] **Step 3: Rewrite `layoutCompressedBlocks` and `writeCompressedBlocks`**

In `builder_compress.go`, replace `layoutCompressedBlocks` (`:237-243`) and `writeCompressedBlocks` (`:246-254`):

```go
// layoutCompressedBlocks assigns absolute block addresses to the inode's dense
// block list and rewrites blkaddr in block-owning index entries (HEAD/PLAIN).
// NONHEAD entries keep their delta encoding; tail markers keep blkaddr 0.
func (b *Builder) layoutCompressedBlocks(ino *buildInode, cdata *compressedFileData, startBlk *int64) {
	base := *startBlk
	for i := range cdata.indexEntries {
		if cdata.blockOf[i] >= 0 {
			cdata.indexEntries[i].Union = uint32(base + int64(cdata.blockOf[i]))
		}
	}
	ino.startBlk = uint64(base)
	*startBlk += int64(len(cdata.blocks))
}

// writeCompressedBlocks writes the inode's dense block list contiguously
// starting at ino.startBlk.
func (b *Builder) writeCompressedBlocks(ino *buildInode, cdata *compressedFileData) error {
	for i, blk := range cdata.blocks {
		off := (int64(ino.startBlk) + int64(i)) * int64(b.blockSize)
		if _, err := b.w.WriteAt(blk, off); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Update the `writeCompressedBlocks` call site**

In `builder.go` (in `writeDataBlocks`), change:

```go
			if err := b.writeCompressedBlocks(cdata); err != nil {
```

to:

```go
			if err := b.writeCompressedBlocks(ino, cdata); err != nil {
```

> `BlocksLo` needs no change here: it is computed from `b.endDataBlk` (set by `layoutDataBlocks`, which advances `currentBlk` through `layoutCompressedBlocks`), so compressed blocks are already counted regardless of the `pclusters`→`blocks` rename.

- [ ] **Step 5: Set the big-pcluster advise bit in `writeCompressedInode`**

In `builder_compress.go`, in `writeCompressedInode`, replace the map-header build (`:204-209`, the `var mh [8]byte` ... `mh[7] = 0` block):

```go
	mapHeaderOff := ino.metaOff + 64
	var mh [8]byte
	// h_fragmentoff = 0 (no fragments)
	// h_advise = 0 (no special flags)
	// h_algorithmtype = selected algorithm for HEAD1 (bits 0-3)
	mh[6] = b.comprAlgID() // h_algorithmtype (HEAD1)
	mh[7] = 0              // h_clusterbits = 0 (lcluster = block_size)
```

with:

```go
	mapHeaderOff := ino.metaOff + 64
	// h_advise: mark BIG_PCLUSTER_1 when any pcluster spans multiple lclusters.
	var advise uint16
	for _, e := range cdata.indexEntries {
		if e.Type() == ondisk.LClusterTypeNonHead {
			advise |= ondisk.AdviseBigPCluster1
			break
		}
	}
	var mh [8]byte
	binary.LittleEndian.PutUint16(mh[4:], advise) // h_advise
	mh[6] = b.comprAlgID()                         // h_algorithmtype (HEAD1)
	mh[7] = 0                                       // h_clusterbits = 0 (lcluster == block)
```

- [ ] **Step 6: Set the big-pcluster incompat feature bit**

In `builder.go`, replace the compressed branch of `computeIncompatFeatures` (`:954-959`):

```go
	if len(b.compressedData) > 0 {
		flags |= ondisk.FeatureIncompatZeroPadding
		if b.compression == CompressionZstd {
			flags |= ondisk.FeatureIncompatComprCfgs
		}
	}
```

with:

```go
	if len(b.compressedData) > 0 {
		flags |= ondisk.FeatureIncompatZeroPadding
		if b.compression == CompressionZstd {
			flags |= ondisk.FeatureIncompatComprCfgs
		}
		for _, cdata := range b.compressedData {
			big := false
			for _, e := range cdata.indexEntries {
				if e.Type() == ondisk.LClusterTypeNonHead {
					big = true
					break
				}
			}
			if big {
				// Same bit as COMPR_CFGS (0x2); harmless to OR again.
				flags |= ondisk.FeatureIncompatBigPCluster
				break
			}
		}
	}
```

- [ ] **Step 7: Point the zstd encoder window at the pcluster size**

In `builder_compress.go`, replace the window line in `zstdEncoder` (`:263-266`):

```go
	window := b.blockSize
	if window < 1024 { // zstd minimum window size
		window = 1024
	}
```

with:

```go
	window := b.pclusterSizeEff()
	if window < 1024 { // zstd minimum window size
		window = 1024
	}
```

Also update the `zstdEncoder` doc comment (`:256-258`) to read:

```go
// zstdEncoder returns a reusable zstd encoder whose window tracks the pcluster
// size, so back-references can span the whole compression unit (a big pcluster
// covering several lclusters).
```

- [ ] **Step 8: Derive the zstd cfgs windowlog from the pcluster size**

In `builder.go`, replace the windowlog computation in `writeComprCfgs` (`:996-1004`):

```go
	// The zstd window log equals the block-size bits for this builder
	// (h_clusterbits = 0, so each extent decompresses to one block). The
	// encoder floors the window at 1024 bytes, so clamp to zstd's minimum
	// window log (ZSTD_WINDOWLOG_ABSOLUTEMIN = 10) to match and avoid underflow.
	wbits := b.blkSzBits
	if wbits < 10 {
		wbits = 10
	}
	cfg := ondisk.ZstdCfgs{WindowLog: wbits - 10}
```

with:

```go
	// A big pcluster decompresses to pclusterSize bytes, so the zstd window
	// must track the pcluster size (the encoder uses the same window). On disk
	// the window log is stored minus ZSTD_WINDOWLOG_ABSOLUTEMIN (10).
	wbits := int(b.pclusterBitsEff())
	if wbits < 10 {
		wbits = 10
	}
	cfg := ondisk.ZstdCfgs{WindowLog: uint8(wbits - 10)}
```

- [ ] **Step 9: Update the two zstd cfgs windowlog tests**

In `builder_test.go`, `TestBuilderZstdComprCfgs` (`:635-638`): block size bits 12 → pcluster bits 16 → windowlog 6. Replace:

```go
	// blkSzBits 12, window log 12, on-disk = 12 - 10 = 2.
	if windowLog != 2 {
		t.Fatalf("zstd cfgs windowlog = %d, want 2", windowLog)
	}
```

with:

```go
	// blkSzBits 12, default pcluster bits 16, on-disk = 16 - 10 = 6.
	if windowLog != 6 {
		t.Fatalf("zstd cfgs windowlog = %d, want 6", windowLog)
	}
```

In `TestBuilderZstdComprCfgsBlockSize` (`:681-684`): blkSzBits 14 → pcluster bits 18 → windowlog 8. Replace:

```go
	// windowlog on disk = blkSzBits - ZSTD_WINDOWLOG_ABSOLUTEMIN(10) = 4.
	if windowLog := img[off+3]; windowLog != blkSzBits-10 {
		t.Fatalf("zstd cfgs windowlog = %d, want %d", windowLog, blkSzBits-10)
	}
```

with:

```go
	// pcluster bits = blkSzBits+4 = 18; windowlog on disk = 18 - 10 = 8.
	if windowLog := img[off+3]; windowLog != blkSzBits-10+4 {
		t.Fatalf("zstd cfgs windowlog = %d, want %d", windowLog, blkSzBits-10+4)
	}
```

- [ ] **Step 10: Relax the mixed-content structural assertion**

`TestBuilderZstdMixedContent` (`builder_test.go:489-577`) asserts per-lcluster `HEAD1`+`PLAIN` mixing inside one inode, which no longer holds under default grouping (a dedicated structural test is added in Task 5). Remove the structural block (`:539-562`, from the `// Verify the mixed file...` comment through the `head1 == 0 || plain == 0` check), keeping the build and the round-trip read-back loop (`:564-577`). The function should end with only the `Open` + per-file `ReadFile`/`bytes.Equal` verification.

- [ ] **Step 11: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: all PASS (existing round-trips, the updated cfgs tests, the relaxed mixed test, Task 1/2 tests).

- [ ] **Step 12: Commit**

```bash
go tool goimports -w .
git add builder.go builder_compress.go builder_test.go
git commit -m "feat(builder): pack compressed data into big pclusters"
```

---

### Task 5: Builder big-pcluster correctness tests (encoding, size, round-trips)

Prove the builder emits the right encoding, actually saves space, and round-trips every layout through the (fixed) reader.

**Files:**
- Modify: `builder_bigpcluster_test.go`

- [ ] **Step 1: Write the encoding assertion test**

Append to `builder_bigpcluster_test.go`:

```go
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

func TestBuilderBigPClusterEncoding(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(1 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// Highly compressible, not block-aligned -> single big pcluster with a tail marker.
	data := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog.\n"), 3000) // 135000 bytes
	b.AddFile("/big.txt", &staticFileInfo{
		name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch,
	}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	cdata := findCompressed(t, b, "/big.txt")
	if len(cdata.indexEntries) != 33 { // ceil(135000/4096)
		t.Fatalf("indexEntries = %d, want 33", len(cdata.indexEntries))
	}
	// Far fewer physical blocks than logical lclusters.
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
	if last.Type() != ondisk.LClusterTypePlain || last.ClusterOfs != uint16(135000%4096) {
		t.Fatalf("tail marker = type %d clusterofs %d, want PLAIN %d", last.Type(), last.ClusterOfs, 135000%4096)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -run TestBuilderBigPClusterEncoding -v .`
Expected: PASS.

- [ ] **Step 3: Write the size-regression test (the point of issue #5)**

Append:

```go
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
		uncompressed := build()
		compressed := build(WithCompression(alg))
		uBlk, cBlk := blocksLo(uncompressed), blocksLo(compressed)
		if cBlk >= uBlk {
			t.Errorf("alg %d: compressed BlocksLo %d not < uncompressed %d", alg, cBlk, uBlk)
		}
		t.Logf("alg %d: uncompressed=%d blocks, compressed=%d blocks", alg, uBlk, cBlk)
	}
}
```

- [ ] **Step 4: Run it**

Run: `go test -run TestBuilderBigPClusterSavesSpace -v .`
Expected: PASS — compressed `BlocksLo` is far smaller than uncompressed for both LZ4 and zstd.

- [ ] **Step 5: Write round-trip tests for every layout**

Append:

```go
func TestBuilderBigPClusterRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	mkdata := func() map[string][]byte {
		// block-aligned compressible (no tail marker)
		aligned := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB exactly
		// unaligned compressible (tail marker)
		unaligned := bytes.Repeat([]byte("compress me well\n"), 9000)
		// spans several pclusters at default 16-lcluster grouping
		multi := bytes.Repeat([]byte("many pclusters worth of repeating text\n"), 60000)
		return map[string][]byte{
			"/aligned.txt":   aligned,
			"/unaligned.txt": unaligned,
			"/multi.txt":     multi,
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

func TestBuilderBigPClusterMixed(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(1 << 20)

	// 2-lcluster pclusters: a compressible pair (HEAD1+NONHEAD) followed by an
	// incompressible pair (PLAIN+PLAIN) in the same inode.
	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch),
		WithCompression(CompressionAutoLZ4), WithPClusterSize(13))
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
```

Add `"math/rand"` to the test file's import block.

- [ ] **Step 6: Run the new tests and the full suite**

Run: `go test -run TestBuilderBigPCluster -v .` then `go test ./...`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
go tool goimports -w .
git add builder_bigpcluster_test.go
git commit -m "test(builder): cover big-pcluster encoding, size savings, and round-trips"
```

---

### Task 6: Validate builder LZ4 output with `fsck.erofs`

The LZ4 path can be checked against reference tooling locally; this is the strongest available guard against subtly-wrong index encoding. Skips cleanly when `fsck.erofs` is absent (e.g. CI).

**Files:**
- Modify: `builder_bigpcluster_test.go`

- [ ] **Step 1: Write the fsck validation test**

Append:

```go
import (
	"os/exec"
	"path/filepath"
) // merge into the existing import block

func TestBuilderBigPClusterFsck(t *testing.T) {
	fsck, err := exec.LookPath("fsck.erofs")
	if err != nil {
		t.Skip("fsck.erofs not installed")
	}
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := newWriterAtBuffer(8 << 20)

	b := NewBuilder(buf, WithBlockSize(12), WithEpoch(epoch), WithCompression(CompressionAutoLZ4))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	data := bytes.Repeat([]byte("fsck should accept this big-pcluster LZ4 image\n"), 40000)
	b.AddFile("/big.txt", &staticFileInfo{name: "big.txt", mode: 0o644, size: int64(len(data)), mod: epoch}, data)
	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	path := filepath.Join(t.TempDir(), "out.img")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(fsck, "--extract", path).CombinedOutput()
	if err != nil {
		t.Fatalf("fsck.erofs failed: %v\n%s", err, out)
	}
}
```

> Note: `fsck.erofs --extract` fully decompresses and verifies every file (not just structural checks), so a passing run validates the index encoding end-to-end. If `--extract` is unavailable on the local build, fall back to plain `exec.Command(fsck, path)`.

- [ ] **Step 2: Run it**

Run: `go test -run TestBuilderBigPClusterFsck -v .`
Expected: PASS (or SKIP if `fsck.erofs` is missing). If it FAILS, the index encoding diverges from the kernel's expectation — debug against the captured `mkfs.erofs` ground truth in the Background section before proceeding.

- [ ] **Step 3: Commit**

```bash
go tool goimports -w .
git add builder_bigpcluster_test.go
git commit -m "test(builder): validate LZ4 big-pcluster output with fsck.erofs"
```

---

### Task 7: Update docs and changelog

**Files:**
- Modify: `CLAUDE.md` (Project section), `CHANGELOG.md`

- [ ] **Step 1: Update `CLAUDE.md`**

In `CLAUDE.md`, replace the final sentence of the `## Project` section:

```
Output is intended to be bytewise compatible with the Linux kernel EROFS driver and mkfs.erofs; the zstd compr_cfgs output has not yet been validated against erofs-utils >= 1.8 (the toolchain that first supports zstd — 1.7.1 does not).
```

with:

```
The builder packs compressed data into big pclusters (one pcluster covering several lclusters, occupying fewer physical blocks than its logical span) so compressed images are actually smaller. Output is intended to be bytewise compatible with the Linux kernel EROFS driver and mkfs.erofs: LZ4 big-pcluster output is validated locally with fsck.erofs 1.7.1, but the zstd path (compr_cfgs + zstd big pclusters) has not yet been validated against erofs-utils >= 1.8 (the toolchain that first supports zstd — 1.7.1 does not).
```

- [ ] **Step 2: Add a `CHANGELOG.md` entry**

Add an `Unreleased` section at the top of `CHANGELOG.md` (match the existing format):

```markdown
## Unreleased

### Added
- Big-pcluster packing in the builder: compressed files now occupy roughly
  `ceil(compressed_len / block_size)` physical blocks per pcluster instead of one
  block per lcluster, so compressed images are smaller than uncompressed ones.
  Tunable via `WithPClusterSize`. (#5)

### Fixed
- Reader: correctly decode big-pcluster compressed images (extent length,
  `D0_CBLKCNT` handling, NONHEAD lookback, and partial-tail random access). (#5)
- Builder: `BlocksLo` in the superblock now counts compressed data blocks. (#5)
```

- [ ] **Step 3: Verify the suite once more**

Run: `go test ./...`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md CHANGELOG.md
git commit -m "docs: document big-pcluster packing and remaining zstd validation caveat"
```

---

## Self-Review

**Spec coverage** (issue #5 body + scoping comment):
- "Pad every lcluster to a full block / no packing" → Task 4 (grouped compression + dense `blocks`).
- "Group lclusters into pclusters, pick a pclustersize" → Task 3 (`WithPClusterSize`, default 16 lclusters) + Task 4 grouping loop.
- "zstd window/cfgs windowlog from pclustersize" → Task 4 steps 7–9.
- "Pack densely across `ceil(compressed_len/block)` blocks" → Task 4 steps 1–4.
- "Emit HEAD1 + (K-1) NONHEAD with deltas and D0_CBLKCNT matching `z_erofs_lcluster_index`" → Task 4 step 2 (encoding matches captured `mkfs.erofs` ground truth) + Task 5 step 1 (assertions).
- "Set `Z_EROFS_ADVISE_BIG_PCLUSTER` + FeatureIncompat big-pcluster" → Task 4 steps 5–6.
- "Keep per-block PLAIN fallback for incompressible regions" → Task 4 step 2 (PLAIN-group branch) + Task 5 `TestBuilderBigPClusterMixed`.
- "Decide interaction with h_clusterbits" → kept at 0 (documented in Background; big pcluster is signalled via D0_CBLKCNT + advise, orthogonal to clusterbits, exactly as `mkfs.erofs` does).
- "BlocksLo fix, independent, do first" → Task 1 (standalone, its own commit).
- "Verify/repair the reader against a real big-pcluster image" → Task 2 (committed real `mkfs.erofs` fixtures; four bugs fixed and verified).
- "Round-trip every layout (single-block, multi-block, mixed, tail)" → Task 5 (`RoundTrip`, `Mixed`, `Encoding` with tail marker; C=1 and C=11 fixtures in Task 2).
- "Size assertions: fewer data blocks than uncompressed" → Task 5 `TestBuilderBigPClusterSavesSpace`.
- "Validate against fsck.erofs ≥ 1.8 / kernel where feasible" → Task 6 (LZ4 via local fsck.erofs 1.7.1); zstd deferred and documented (Task 7), matching the issue's tooling caveat.

**Placeholder scan:** none — every code step contains complete code; commands have expected outcomes.

**Type consistency:** `compressedFileData` fields (`blocks`, `indexEntries`, `blockOf`, `lclusterSize`) are introduced in Task 4 step 1 and used consistently in Task 4 steps 3–4 and Task 5; `pclusterBitsEff`/`pclusterSizeEff`/`pclusterLclustersEff` defined in Task 3 and used in Task 4; `blocksLo`/`findCompressed` test helpers defined once and reused. `writeCompressedBlocks` signature change (`ino, cdata`) is matched at its only call site (Task 4 step 4). The Task 1 `len(cdata.pclusters)` is explicitly migrated to `len(cdata.blocks)` in Task 4 step 4.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-28-big-pcluster-packing.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
