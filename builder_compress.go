package erofs

import (
	"encoding/binary"
	"path/filepath"
	"strings"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// CompressionAlgorithm selects which compression algorithm to use.
type CompressionAlgorithm int

const (
	CompressionNone    CompressionAlgorithm = -1
	CompressionAutoLZ4 CompressionAlgorithm = CompressionAlgorithm(ondisk.CompressionLZ4)
	CompressionZstd    CompressionAlgorithm = CompressionAlgorithm(ondisk.CompressionZstd)
)

// WithCompression enables compression during image creation.
// Files that compress well will use compressed layout; incompressible
// files fall back to flat storage automatically.
func WithCompression(alg CompressionAlgorithm) BuildOption {
	return func(b *Builder) {
		b.compression = alg
		b.compressEnabled = true
	}
}

// WithCompressionLevel sets the compression level; higher is smaller but
// slower. For Zstandard it is a numeric level (1..22, as in `zstd -N`), mapped
// to the nearest level the encoder supports. For LZ4 any level > 0 switches the
// builder to the high-compression encoder (lz4hc), with the level clamped to
// lz4's 1..9 search depth. It only affects encoding, so it does not change
// on-disk compatibility. A level of 0 keeps the algorithm's default.
func WithCompressionLevel(level int) BuildOption {
	return func(b *Builder) {
		b.compressionLevel = level
	}
}

// incompressibleExts is a set of file extensions known to be already compressed.
var incompressibleExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true,
	".gz": true, ".bz2": true, ".xz": true, ".zst": true, ".lz4": true, ".br": true,
	".zip": true, ".7z": true, ".rar": true, ".tar.gz": true, ".tar.xz": true,
	".mp4": true, ".webm": true, ".mkv": true, ".avi": true, ".mov": true,
	".mp3": true, ".ogg": true, ".opus": true, ".flac": true, ".aac": true,
	".woff2": true, ".woff": true,
}

// isIncompressible returns true if the file is unlikely to benefit from compression.
func isIncompressible(path string, size int64) bool {
	if size == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	return incompressibleExts[ext]
}

// tryCompressFile attempts to compress file data using the selected compression algorithm.
// Returns the compressed lcluster data and the FULL index entries,
// or nil if the file should remain uncompressed.
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

	// Only LZ4 and Zstandard are supported; anything else stores the file
	// uncompressed rather than failing the build.
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
			// EROFS 0-padding: the compressed stream is right-aligned within
			// its physical blocks (leading zero padding). Readers locate the
			// stream by skipping leading zeros, so the remaining length is the
			// exact compressed size -- which liblz4's partial decode requires.
			headBlock := len(blocks)
			packed := make([]byte, cBlocks*bs)
			copy(packed[len(packed)-len(comp):], comp)
			for i := range cBlocks {
				blocks = append(blocks, packed[i*bs:(i+1)*bs])
			}
			lastLcn := groupEnd - 1
			tailMarker := groupEnd == totalLclusters && tail != 0
			for j := range spanLcl {
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
			for j := range spanLcl {
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

// compressGroup compresses one pcluster's worth of bytes as a single unit.
// Returns (compressed, true) when compression produced output; (nil, false)
// when the data is incompressible (LZ4 reports no gain).
func (b *Builder) compressGroup(group []byte) ([]byte, bool) {
	switch b.compression {
	case CompressionAutoLZ4:
		dst := make([]byte, lz4.CompressBlockBound(len(group)))
		var n int
		var err error
		if b.compressionLevel > 0 {
			n, err = b.lz4HCCompressor().CompressBlock(group, dst)
		} else {
			n, err = lz4.CompressBlock(group, dst, nil)
		}
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

// computeCompressedMetaSize returns the metadata size for a compressed inode.
// Layout: 64-byte inode + ALIGN(metaEnd, 8) padding + 8-byte map header + 8-byte padding + N*8-byte index
func computeCompressedMetaSize(numLclusters int) int {
	// inode (64) + alignment to 8 (already 64, no padding needed) + map header (8) + padding (8) + index entries
	return 64 + 8 + 8 + numLclusters*8
}

// writeCompressedInode writes a compressed inode's metadata.
func (b *Builder) writeCompressedInode(ino *buildInode, cdata *compressedFileData) error {
	mtSec, mtNsec := b.diskMtime(ino.mtime)
	ei := ondisk.InodeExtended{
		Format:    uint16(ondisk.InodeLayoutExtended) | uint16(ondisk.InodeCompressedFull)<<ondisk.IDataLayoutBit,
		Mode:      erofsModeFromFS(ino.mode),
		Size:      uint64(ino.size),
		U:         uint32(len(cdata.blocks)), // blocks_lo = total physical blocks
		UID:       ino.uid,
		GID:       ino.gid,
		Mtime:     mtSec,
		MtimeNsec: mtNsec,
		NLink:     1,
		NB:        0, // extended inode: offset-6 is startblk_hi/blocks_hi, not nlink
	}
	ei.Ino = uint32(ino.nid)

	// Write inode.
	inodeBuf := make([]byte, 64)
	buf := inodeBuf
	binary.LittleEndian.PutUint16(buf[0:], ei.Format)
	binary.LittleEndian.PutUint16(buf[2:], ei.XattrICount)
	binary.LittleEndian.PutUint16(buf[4:], ei.Mode)
	binary.LittleEndian.PutUint16(buf[6:], ei.NB)
	binary.LittleEndian.PutUint64(buf[8:], ei.Size)
	binary.LittleEndian.PutUint32(buf[16:], ei.U)
	binary.LittleEndian.PutUint32(buf[20:], ei.Ino)
	binary.LittleEndian.PutUint32(buf[24:], ei.UID)
	binary.LittleEndian.PutUint32(buf[28:], ei.GID)
	binary.LittleEndian.PutUint64(buf[32:], uint64(ei.Mtime))
	binary.LittleEndian.PutUint32(buf[40:], ei.MtimeNsec)
	binary.LittleEndian.PutUint32(buf[44:], ei.NLink)

	if _, err := b.w.WriteAt(inodeBuf, ino.metaOff); err != nil {
		return err
	}

	// Write map header at ALIGN(metaEnd, 8) = metaOff + 64 (already aligned).
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
	mh[6] = b.comprAlgID()                        // h_algorithmtype (HEAD1)
	mh[7] = 0                                     // h_clusterbits = 0 (lcluster == block)
	if _, err := b.w.WriteAt(mh[:], mapHeaderOff); err != nil {
		return err
	}

	// Write 8 bytes of padding.
	var pad [8]byte
	if _, err := b.w.WriteAt(pad[:], mapHeaderOff+8); err != nil {
		return err
	}

	// Write FULL index entries (8 bytes each).
	indexOff := mapHeaderOff + 16 // after map header + padding
	for i, entry := range cdata.indexEntries {
		var idx [8]byte
		binary.LittleEndian.PutUint16(idx[0:], entry.Advise)
		binary.LittleEndian.PutUint16(idx[2:], entry.ClusterOfs)
		binary.LittleEndian.PutUint32(idx[4:], entry.Union)
		if _, err := b.w.WriteAt(idx[:], indexOff+int64(i)*8); err != nil {
			return err
		}
	}

	return nil
}

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

// lz4HCCompressor returns a reusable high-compression LZ4 compressor whose
// search depth tracks the requested compression level, clamped to lz4's 1..9
// range. The HC encoder produces standard LZ4 blocks, so it does not change
// on-disk compatibility.
func (b *Builder) lz4HCCompressor() *lz4.CompressorHC {
	if b.lz4HC != nil {
		return b.lz4HC
	}
	depth := b.compressionLevel
	if depth < 1 {
		depth = 1
	}
	if depth > 9 {
		depth = 9
	}
	// lz4.Level1..Level9 are defined as 1 << (8 + n).
	b.lz4HC = &lz4.CompressorHC{Level: lz4.CompressionLevel(1 << (8 + depth))}
	return b.lz4HC
}

// zstdEncoder returns a reusable zstd encoder whose window tracks the pcluster
// size, so back-references can span the whole compression unit (a big pcluster
// covering several lclusters).
func (b *Builder) zstdEncoder() (*zstd.Encoder, error) {
	if b.zstdEnc != nil {
		return b.zstdEnc, nil
	}
	window := b.pclusterSizeEff()
	if window < 1024 { // zstd minimum window size
		window = 1024
	}
	level := zstd.SpeedDefault
	if b.compressionLevel != 0 {
		// Map a numeric zstd level (1..22, as in `zstd -N`) to klauspost's
		// nearest encoder level. The level affects only the encoder; the frame
		// stays within the advertised window, so it does not change on-disk
		// compatibility.
		level = zstd.EncoderLevelFromZstd(b.compressionLevel)
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(level),
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
