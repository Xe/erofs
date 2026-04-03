package erofs

import (
	"encoding/binary"
	"path/filepath"
	"strings"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/pierrec/lz4/v4"
)

// CompressionAlgorithm selects which compression algorithm to use.
type CompressionAlgorithm int

const (
	CompressionNone    CompressionAlgorithm = -1
	CompressionAutoLZ4 CompressionAlgorithm = CompressionAlgorithm(ondisk.CompressionLZ4)
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

// compressedBlock holds the result of compressing one lcluster.
type compressedBlock struct {
	data       []byte
	compressed bool // true if actually compressed, false if stored plain
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

// tryCompressFile attempts to compress file data using LZ4.
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

	data := ino.data
	lclusterSize := b.blockSize // h_clusterbits = 0
	totalLclusters := (len(data) + lclusterSize - 1) / lclusterSize

	var pclusters [][]byte
	var indexEntries []ondisk.LClusterIndex
	anyCompressed := false

	for i := range totalLclusters {
		start := i * lclusterSize
		end := start + lclusterSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]

		// Try LZ4 compression.
		maxOut := lz4.CompressBlockBound(len(chunk))
		compressed := make([]byte, maxOut)
		n, err := lz4.CompressBlock(chunk, compressed, nil)

		if err != nil || n <= 0 || n >= len(chunk) {
			// Compression didn't help -- store as PLAIN type.
			// Pad to block size.
			pcluster := make([]byte, b.blockSize)
			copy(pcluster, chunk)
			pclusters = append(pclusters, pcluster)
			indexEntries = append(indexEntries, ondisk.LClusterIndex{
				Advise:     ondisk.LClusterTypePlain,
				ClusterOfs: 0,
				Union:      0, // blkaddr filled later
			})
		} else {
			// Compression worked. Pad compressed data to block size.
			pcluster := make([]byte, b.blockSize)
			copy(pcluster, compressed[:n])
			pclusters = append(pclusters, pcluster)
			indexEntries = append(indexEntries, ondisk.LClusterIndex{
				Advise:     ondisk.LClusterTypeHead1,
				ClusterOfs: 0,
				Union:      0, // blkaddr filled later
			})
			anyCompressed = true
		}
	}

	if !anyCompressed {
		// Nothing compressed at all -- don't bother with compressed layout.
		return nil, false
	}

	return &compressedFileData{
		pclusters:    pclusters,
		indexEntries: indexEntries,
		lclusterSize: lclusterSize,
	}, true
}

// compressedFileData holds the compression results for a single file.
type compressedFileData struct {
	pclusters    [][]byte
	indexEntries []ondisk.LClusterIndex
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
	ei := ondisk.InodeExtended{
		Format:    uint16(ondisk.InodeLayoutExtended) | uint16(ondisk.InodeCompressedFull)<<ondisk.IDataLayoutBit,
		Mode:      erofsModeFromFS(ino.mode),
		Size:      uint64(ino.size),
		U:         uint32(len(cdata.pclusters)), // blocks_lo = total compressed blocks
		UID:       ino.uid,
		GID:       ino.gid,
		Mtime:     ino.mtime.Unix() - b.epoch,
		MtimeNsec: uint32(ino.mtime.Nanosecond()),
		NLink:     1,
		NB:        1,
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
	var mh [8]byte
	// h_fragmentoff = 0 (no fragments)
	// h_advise = 0 (no special flags)
	// h_algorithmtype = LZ4 for HEAD1 (bits 0-3)
	mh[6] = ondisk.CompressionLZ4 // h_algorithmtype
	mh[7] = 0                     // h_clusterbits = 0 (lcluster = block_size)
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

// layoutCompressedBlocks assigns block addresses to compressed pclusters and
// updates the index entries with the correct blkaddr values.
func (b *Builder) layoutCompressedBlocks(ino *buildInode, cdata *compressedFileData, startBlk *int64) {
	for i := range cdata.indexEntries {
		cdata.indexEntries[i].Union = uint32(*startBlk)
		*startBlk++
	}
	ino.startBlk = uint64(cdata.indexEntries[0].Union)
}

// writeCompressedBlocks writes the pcluster data to disk.
func (b *Builder) writeCompressedBlocks(cdata *compressedFileData) error {
	for i, entry := range cdata.indexEntries {
		off := int64(entry.Union) * int64(b.blockSize)
		if _, err := b.w.WriteAt(cdata.pclusters[i], off); err != nil {
			return err
		}
	}
	return nil
}
