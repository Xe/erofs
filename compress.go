package erofs

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// compressedFile implements fs.File for compressed EROFS files.
type compressedFile struct {
	fsys   *FS
	ino    *inode
	name   string
	offset int64
	closed bool

	// Compression metadata.
	mapHeader ondisk.MapHeader
	lclustSz  int64 // lcluster size in bytes

	// Cache for decompressed pcluster.
	cacheStart int64  // logical offset of cached extent start
	cacheEnd   int64  // logical offset of cached extent end
	cacheData  []byte // decompressed data
}

func (f *FS) openCompressedFile(ino *inode, name string) (fs.File, error) {
	// Read the compression map header.
	headerPos := align(ino.metaEnd(), 8)
	var mh ondisk.MapHeader
	buf := make([]byte, 8)
	if _, err := f.r.ReadAt(buf, headerPos); err != nil {
		return nil, fmt.Errorf("erofs: reading map header: %w", err)
	}
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &mh); err != nil {
		return nil, fmt.Errorf("erofs: decoding map header: %w", err)
	}

	lclustBits := f.blockSzBits + mh.LClusterBits()
	lclustSz := int64(1) << lclustBits

	return &compressedFile{
		fsys:      f,
		ino:       ino,
		name:      name,
		mapHeader: mh,
		lclustSz:  lclustSz,
	}, nil
}

func (cf *compressedFile) Stat() (fs.FileInfo, error) {
	return cf.ino.fileInfo(baseName(cf.name)), nil
}

func (cf *compressedFile) Read(p []byte) (int, error) {
	if cf.closed {
		return 0, fs.ErrClosed
	}
	if cf.offset >= int64(cf.ino.size) {
		return 0, io.EOF
	}

	n, err := cf.readAt(p, cf.offset)
	cf.offset += int64(n)
	return n, err
}

func (cf *compressedFile) Seek(offset int64, whence int) (int64, error) {
	if cf.closed {
		return 0, fs.ErrClosed
	}
	var newOff int64
	switch whence {
	case io.SeekStart:
		newOff = offset
	case io.SeekCurrent:
		newOff = cf.offset + offset
	case io.SeekEnd:
		newOff = int64(cf.ino.size) + offset
	default:
		return 0, fmt.Errorf("erofs: invalid whence %d", whence)
	}
	if newOff < 0 {
		return 0, fmt.Errorf("erofs: negative seek position")
	}
	cf.offset = newOff
	return newOff, nil
}

func (cf *compressedFile) Close() error {
	cf.closed = true
	cf.cacheData = nil
	return nil
}

func (cf *compressedFile) readAt(p []byte, off int64) (int, error) {
	if off >= int64(cf.ino.size) {
		return 0, io.EOF
	}

	remaining := int64(cf.ino.size) - off
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	total := 0
	for len(p) > 0 {
		// Check cache.
		if off >= cf.cacheStart && off < cf.cacheEnd && cf.cacheData != nil {
			cacheOff := off - cf.cacheStart
			n := copy(p, cf.cacheData[cacheOff:])
			total += n
			off += int64(n)
			p = p[n:]
			continue
		}

		// Decompress the pcluster covering this offset.
		if err := cf.loadPCluster(off); err != nil {
			return total, err
		}
	}

	if off >= int64(cf.ino.size) {
		return total, io.EOF
	}
	return total, nil
}

// loadPCluster loads and decompresses the pcluster covering logical offset la.
func (cf *compressedFile) loadPCluster(la int64) error {
	ino := cf.ino

	// Determine index format.
	switch ino.dataLayout {
	case ondisk.InodeCompressedFull:
		return cf.loadPClusterFull(la)
	case ondisk.InodeCompressedCompact:
		return cf.loadPClusterCompact(la)
	default:
		return fmt.Errorf("erofs: unsupported compressed layout %d", ino.dataLayout)
	}
}

// loadPClusterFull handles the FULL index format (layout 1).
func (cf *compressedFile) loadPClusterFull(la int64) error {
	ino := cf.ino
	f := cf.fsys
	blockSize := int64(f.blockSize)

	// Index start: ALIGN(metaEnd, 8) + 8 (map header) + 8 (padding).
	indexStart := align(ino.metaEnd(), 8) + 8 + 8

	lcn := la / cf.lclustSz

	// Read the lcluster entry.
	entry, err := cf.readLClusterEntry(indexStart, lcn)
	if err != nil {
		return err
	}

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

	// Follow NONHEAD delta chain to find HEAD.
	headLcn := lcn
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
		if headLcn < 0 {
			return fmt.Errorf("erofs: NONHEAD chain went negative at lcn=%d", lcn)
		}
		entry, err = cf.readLClusterEntry(indexStart, headLcn)
		if err != nil {
			return err
		}
	}

	// Determine algorithm.
	ltype := entry.Type()
	var algID uint8
	switch ltype {
	case ondisk.LClusterTypePlain:
		algID = 0xFF // plain/uncompressed
	case ondisk.LClusterTypeHead1:
		algID = cf.mapHeader.HeadAlgorithm()
	case ondisk.LClusterTypeHead2:
		algID = cf.mapHeader.Head2Algorithm()
	default:
		return fmt.Errorf("erofs: unexpected lcluster type %d", ltype)
	}

	pblk := int64(entry.BlkAddr())
	clusterOfs := int64(entry.ClusterOfs)

	// Determine pcluster size.
	// Check if big pcluster: look at next lcluster entry.
	pclusterBlocks := int64(1)
	nextLcn := headLcn + 1
	totalLclusters := (int64(ino.size) + cf.lclustSz - 1) / cf.lclustSz

	if nextLcn < totalLclusters {
		nextEntry, err := cf.readLClusterEntry(indexStart, nextLcn)
		if err == nil && nextEntry.Type() == ondisk.LClusterTypeNonHead &&
			nextEntry.Delta0()&ondisk.LID0CBlkCnt != 0 {
			// D0_CBLKCNT (bit 11) of the first NONHEAD's delta[0] carries the
			// pcluster's compressed block count.
			pclusterBlocks = int64(nextEntry.Delta0() &^ uint16(ondisk.LID0CBlkCnt))
		}
	}
	if pclusterBlocks < 1 {
		pclusterBlocks = 1
	}

	pclusterSize := pclusterBlocks * blockSize
	pa := pblk * blockSize

	// Determine decompressed extent boundaries.
	extentStart := headLcn*cf.lclustSz + clusterOfs
	// Find extent end: scan forward for next HEAD.
	extentEnd := int64(ino.size)
	for scanLcn := headLcn + 1; scanLcn < totalLclusters; scanLcn++ {
		scanEntry, err := cf.readLClusterEntry(indexStart, scanLcn)
		if err != nil {
			break
		}
		if scanEntry.Type() != ondisk.LClusterTypeNonHead {
			// The next non-NONHEAD entry marks the end of this extent; its
			// clusterofs is how far the extent reaches into that lcluster
			// (non-zero for a partial-tail boundary marker).
			extentEnd = scanLcn*cf.lclustSz + int64(scanEntry.ClusterOfs)
			break
		}
	}

	decompSize := extentEnd - extentStart
	if decompSize <= 0 {
		decompSize = cf.lclustSz
	}

	// Read compressed data.
	compressed := make([]byte, pclusterSize)
	n, err := f.r.ReadAt(compressed, pa)
	if err != nil && n == 0 {
		return fmt.Errorf("erofs: reading pcluster at 0x%X: %w", pa, err)
	}
	compressed = compressed[:n]

	// Decompress.
	decompressed := make([]byte, decompSize)

	if ltype == ondisk.LClusterTypePlain {
		interlaced := cf.mapHeader.Advise&ondisk.AdviseInterlacedPCluster != 0
		if interlaced {
			pageofs := int(clusterOfs) % int(blockSize)
			if pageofs > 0 {
				tailLen := int(blockSize) - pageofs
				copy(decompressed, compressed[len(compressed)-tailLen:])
				copy(decompressed[tailLen:], compressed[:len(compressed)-tailLen])
			} else {
				copy(decompressed, compressed)
			}
		} else {
			// SHIFTED: skip clusterOfs bytes.
			copy(decompressed, compressed[clusterOfs:])
		}
	} else {
		// EROFS 0-padding: the compressed stream is right-aligned within its
		// physical blocks. Skip leading zero padding so the decoder sees the
		// stream start (the LZ4 token / zstd magic, never zero) and the exact
		// remaining length. (Older left-aligned images have no leading zeros,
		// so this is a no-op for them; lz4Decompress trims any trailing pad.)
		for len(compressed) > 0 && compressed[0] == 0 {
			compressed = compressed[1:]
		}
		switch algID {
		case ondisk.CompressionLZ4:
			dn, err := lz4Decompress(compressed, decompressed)
			if err != nil {
				return fmt.Errorf("erofs: lz4 decompress: %w", err)
			}
			decompressed = decompressed[:dn]
		case ondisk.CompressionDeflate:
			dn, err := deflateDecompress(compressed, decompressed)
			if err != nil {
				return fmt.Errorf("erofs: deflate decompress: %w", err)
			}
			decompressed = decompressed[:dn]
		case ondisk.CompressionZstd:
			dn, err := zstdDecompress(compressed, decompressed)
			if err != nil {
				return fmt.Errorf("erofs: zstd decompress: %w", err)
			}
			decompressed = decompressed[:dn]
		default:
			return fmt.Errorf("erofs: unsupported compression algorithm %d", algID)
		}
	}

	cf.cacheStart = extentStart
	cf.cacheEnd = extentStart + int64(len(decompressed))
	cf.cacheData = decompressed
	return nil
}

// loadPClusterCompact handles the COMPACT index format (layout 3).
// For now, fall back to reading the entries as if they were packed 4-byte entries.
func (cf *compressedFile) loadPClusterCompact(la int64) error {
	// The compact format is complex (bit-packed). For an initial implementation,
	// we attempt a simplified approach: read from the compacted index.
	ino := cf.ino
	f := cf.fsys
	blockSize := int64(f.blockSize)

	// ebase = ALIGN(metaEnd, 8) + 8 (map header)
	ebase := align(ino.metaEnd(), 8) + 8

	lcn := la / cf.lclustSz
	totalLclusters := (int64(ino.size) + cf.lclustSz - 1) / cf.lclustSz

	// Compute compacted_4b_initial.
	compacted4bInit := int64((32 - ebase%32) / 4 & 7)

	// Determine which region this lcn falls in.
	var entryOff int64
	var packSize int64

	if lcn < compacted4bInit {
		// 4-byte region (initial).
		// Each pack of 2 lclusters = 12 bytes (2*4 + 4 for base blkaddr).
		packIdx := lcn / 2
		inPack := lcn % 2
		entryOff = ebase + packIdx*12
		packSize = 12
		return cf.decodeCompactPack(entryOff, packSize, 2, int(inPack), la, totalLclusters, blockSize)
	}

	rem := lcn - compacted4bInit

	// Check for 2-byte region.
	compacted2b := int64(0)
	if cf.mapHeader.Advise&ondisk.AdviseCompacted2B != 0 && compacted4bInit < totalLclusters {
		compacted2b = ((totalLclusters - compacted4bInit) / 16) * 16
	}

	afterInit := ebase + ((compacted4bInit+1)/2)*12
	if compacted4bInit%2 != 0 {
		afterInit = ebase + (compacted4bInit/2)*12 + 12
	}

	if rem < compacted2b {
		// 2-byte region.
		packIdx := rem / 16
		inPack := int(rem % 16)
		packOff := afterInit + packIdx*36 // 16*2 + 4 = 36 bytes per pack
		return cf.decodeCompactPack(packOff, 36, 16, inPack, la, totalLclusters, blockSize)
	}

	// Remaining 4-byte region.
	rem2 := rem - compacted2b
	afterCompact2b := afterInit + (compacted2b/16)*36
	packIdx := rem2 / 2
	inPack := int(rem2 % 2)
	entryOff = afterCompact2b + packIdx*12
	packSize = 12
	return cf.decodeCompactPack(entryOff, packSize, 2, inPack, la, totalLclusters, blockSize)
}

// decodeCompactPack decodes a compact index pack and loads the pcluster.
func (cf *compressedFile) decodeCompactPack(packOff, packSize int64, vcnt, idx int, la, totalLclusters, blockSize int64) error {
	ino := cf.ino
	f := cf.fsys

	pack := make([]byte, packSize)
	if _, err := f.r.ReadAt(pack, packOff); err != nil {
		return fmt.Errorf("erofs: reading compact pack: %w", err)
	}

	// Last 4 bytes are the base block address.
	baseBlkAddr := int64(le32(pack[len(pack)-4:]))

	// Decode the entry at index idx.
	lclusterBits := int(cf.mapHeader.LClusterBits()) + int(f.blockSzBits)
	lobits := lclusterBits
	if lobits < 12 {
		lobits = 12
	}

	var amortized int
	if vcnt == 2 {
		amortized = 4
	} else {
		amortized = 2
	}
	encodeBits := ((vcnt * amortized) - 4) * 8 / vcnt

	bitPos := idx * encodeBits
	bytePos := bitPos / 8
	bitOff := uint(bitPos % 8)

	// Read 4 bytes starting at bytePos.
	if bytePos+4 > len(pack)-4 { // don't read into base blkaddr
		bytePos = len(pack) - 8
		if bytePos < 0 {
			bytePos = 0
		}
	}
	raw := le32(pack[bytePos:])
	value := raw >> bitOff

	lo := int64(value) & ((1 << lobits) - 1)
	ltype := uint8((value >> lobits) & 3)

	_ = lo // delta or offset depending on type

	// For HEAD types, compute block address.
	// The base blkaddr is for the last entry in the pack.
	// HEAD entries accumulate backwards from the base.
	if ltype == ondisk.LClusterTypeHead1 || ltype == ondisk.LClusterTypeHead2 || ltype == ondisk.LClusterTypePlain {
		// Simplified: use baseBlkAddr for this entry.
		// A full implementation would sum up block offsets backwards.
		pblk := baseBlkAddr
		pa := pblk * blockSize

		lcn := la / cf.lclustSz
		extentStart := lcn * cf.lclustSz
		extentEnd := extentStart + cf.lclustSz
		if extentEnd > int64(ino.size) {
			extentEnd = int64(ino.size)
		}

		decompSize := extentEnd - extentStart
		pclusterSize := blockSize // default 1 block

		compressed := make([]byte, pclusterSize)
		n, err := f.r.ReadAt(compressed, pa)
		if err != nil && n == 0 {
			return fmt.Errorf("erofs: reading compact pcluster: %w", err)
		}
		compressed = compressed[:n]

		decompressed := make([]byte, decompSize)

		if ltype == ondisk.LClusterTypePlain {
			interlaced := cf.mapHeader.Advise&ondisk.AdviseInterlacedPCluster != 0
			if interlaced {
				pageofs := int(lo) % int(blockSize)
				if pageofs > 0 {
					tailLen := int(blockSize) - pageofs
					copy(decompressed, compressed[len(compressed)-tailLen:])
					copy(decompressed[tailLen:], compressed[:len(compressed)-tailLen])
				} else {
					copy(decompressed, compressed)
				}
			} else {
				copy(decompressed, compressed[lo:])
			}
		} else {
			var algID uint8
			if ltype == ondisk.LClusterTypeHead1 {
				algID = cf.mapHeader.HeadAlgorithm()
			} else {
				algID = cf.mapHeader.Head2Algorithm()
			}

			switch algID {
			case ondisk.CompressionLZ4:
				dn, err := lz4Decompress(compressed, decompressed)
				if err != nil {
					return fmt.Errorf("erofs: lz4 decompress (compact): %w", err)
				}
				decompressed = decompressed[:dn]
			case ondisk.CompressionDeflate:
				dn, err := deflateDecompress(compressed, decompressed)
				if err != nil {
					return fmt.Errorf("erofs: deflate decompress (compact): %w", err)
				}
				decompressed = decompressed[:dn]
			case ondisk.CompressionZstd:
				dn, err := zstdDecompress(compressed, decompressed)
				if err != nil {
					return fmt.Errorf("erofs: zstd decompress (compact): %w", err)
				}
				decompressed = decompressed[:dn]
			default:
				return fmt.Errorf("erofs: unsupported compression algorithm %d", algID)
			}
		}

		cf.cacheStart = extentStart
		cf.cacheEnd = extentStart + int64(len(decompressed))
		cf.cacheData = decompressed
		return nil
	}

	// NONHEAD: need to walk back to find HEAD. Use the FULL index fallback.
	return fmt.Errorf("erofs: compact NONHEAD resolution not yet implemented for lcn=%d", la/cf.lclustSz)
}

// readLClusterEntry reads a single lcluster index entry in FULL format.
func (cf *compressedFile) readLClusterEntry(indexStart, lcn int64) (ondisk.LClusterIndex, error) {
	off := indexStart + lcn*8
	buf := make([]byte, 8)
	if _, err := cf.fsys.r.ReadAt(buf, off); err != nil {
		return ondisk.LClusterIndex{}, err
	}
	return ondisk.LClusterIndex{
		Advise:     le16(buf[0:2]),
		ClusterOfs: le16(buf[2:4]),
		Union:      le32(buf[4:8]),
	}, nil
}

// lz4Decompress decompresses LZ4 block data.
func lz4Decompress(src, dst []byte) (int, error) {
	// Trim trailing zeros if present (ZERO_PADDING feature).
	trimmed := src
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == 0 {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if len(trimmed) == 0 {
		return 0, nil
	}

	n, err := lz4.UncompressBlock(trimmed, dst)
	if err != nil {
		return 0, err
	}
	return n, nil
}

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
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}

// deflateDecompress decompresses DEFLATE data.
func deflateDecompress(src, dst []byte) (int, error) {
	r := flate.NewReader(bytes.NewReader(src))
	defer r.Close()
	n, err := io.ReadFull(r, dst)
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}
