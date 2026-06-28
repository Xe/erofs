package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// inode is an in-memory representation of an EROFS inode.
type inode struct {
	nid        uint64
	format     uint16
	mode       uint16
	size       uint64
	mtime      time.Time
	uid        uint32
	gid        uint32
	nlink      uint32
	ino        uint32
	dataLayout uint8

	// Flat inode fields.
	startBlk uint64

	// Chunk-based fields.
	chunkFormat uint16
	chunkBits   uint8

	// Xattr.
	xattrISize uint32

	// Compression fields.
	compressedBlocks uint64

	// For resolving inline data / metadata references.
	inodeSize  uint32 // 32 or 64
	iloc       int64  // absolute byte offset of inode on disk
	metaBlkOff int64  // meta_blkaddr * block_size
}

// readInode reads an inode by NID from the filesystem.
func (f *FS) readInode(nid uint64) (*inode, error) {
	iloc := f.metaBlkOff + int64(nid<<ondisk.ISlotBits)
	return f.readInodeAt(nid, iloc)
}

func (f *FS) readInodeAt(nid uint64, iloc int64) (*inode, error) {
	// Read enough bytes for an extended inode (64 bytes).
	// It might span a block boundary, so just read 64 bytes.
	buf := make([]byte, 64)
	n, err := f.r.ReadAt(buf, iloc)
	if err != nil && n < 32 {
		return nil, fmt.Errorf("erofs: reading inode nid=%d: %w", nid, err)
	}

	format := binary.LittleEndian.Uint16(buf[0:2])
	if format & ^uint16(ondisk.IAll) != 0 {
		return nil, fmt.Errorf("erofs: invalid inode format 0x%04X for nid=%d", format, nid)
	}

	version := format & ondisk.IVersionMask
	dataLayout := uint8((format >> ondisk.IDataLayoutBit) & ondisk.IDataLayoutMask)
	if dataLayout >= ondisk.InodeDataLayoutMax {
		return nil, fmt.Errorf("erofs: unsupported data layout %d for nid=%d", dataLayout, nid)
	}

	ino := &inode{
		nid:        nid,
		format:     format,
		dataLayout: dataLayout,
		iloc:       iloc,
		metaBlkOff: f.metaBlkOff,
	}

	if version == ondisk.InodeLayoutCompact {
		var ci ondisk.InodeCompact
		if err := binary.Read(bytes.NewReader(buf[:32]), binary.LittleEndian, &ci); err != nil {
			return nil, fmt.Errorf("erofs: decoding compact inode nid=%d: %w", nid, err)
		}
		ino.inodeSize = 32
		ino.mode = ci.Mode
		ino.size = uint64(ci.Size)
		ino.ino = ci.Ino
		ino.uid = uint32(ci.UID)
		ino.gid = uint32(ci.GID)

		// Timestamp: epoch + i_mtime, nsec = fixed_nsec.
		sec := f.epoch + int64(ci.Mtime)
		ino.mtime = time.Unix(sec, int64(f.fixedNsec))

		ino.parseUnion(ci.U, ci.NB, ci.Format, f.blockSzBits)
	} else {
		if n < 64 {
			return nil, fmt.Errorf("erofs: short read for extended inode nid=%d: got %d bytes", nid, n)
		}
		var ei ondisk.InodeExtended
		if err := binary.Read(bytes.NewReader(buf[:64]), binary.LittleEndian, &ei); err != nil {
			return nil, fmt.Errorf("erofs: decoding extended inode nid=%d: %w", nid, err)
		}
		ino.inodeSize = 64
		ino.mode = ei.Mode
		ino.size = ei.Size
		ino.ino = ei.Ino
		ino.uid = ei.UID
		ino.gid = ei.GID
		ino.nlink = ei.NLink
		ino.mtime = time.Unix(ei.Mtime, int64(ei.MtimeNsec))

		ino.parseUnion(ei.U, ei.NB, ei.Format, f.blockSzBits)
	}

	// Xattr inline size.
	xattrICount := binary.LittleEndian.Uint16(buf[2:4])
	if xattrICount > 0 {
		ino.xattrISize = 12 + 4*(uint32(xattrICount)-1)
	}

	return ino, nil
}

// parseUnion interprets the i_u and i_nb union fields based on the data layout.
func (ino *inode) parseUnion(u uint32, nb uint16, format uint16, blkSzBits uint8) {
	// In the extended inode (version bit set) the field at offset 6 is the
	// high bits of the block address (startblk_hi/blocks_hi); nlink lives in
	// i_nlink (offset 44) and has already been set by the caller. In the
	// compact inode, offset 6 carries nlink (optionally reused as startblk_hi
	// via the NLINK_1 bit).
	extended := format&ondisk.IVersionMask != 0

	switch ino.dataLayout {
	case ondisk.InodeFlatPlain, ondisk.InodeFlatInline:
		ino.startBlk = uint64(u)
		switch {
		case extended:
			ino.startBlk |= uint64(nb) << 32
		case format&(1<<ondisk.INLink1Bit) != 0 && !ino.isDir():
			ino.startBlk |= uint64(nb) << 32
			ino.nlink = 1
		default:
			ino.nlink = uint32(nb)
		}

	case ondisk.InodeCompressedFull, ondisk.InodeCompressedCompact:
		ino.compressedBlocks = uint64(u) | uint64(nb)<<32

	case ondisk.InodeChunkBased:
		ino.chunkFormat = uint16(u & 0xFFFF)
		ino.chunkBits = blkSzBits + uint8(ino.chunkFormat&ondisk.ChunkFormatBlkBitsMask)
		if !extended {
			ino.nlink = uint32(nb)
		}
	}
}

func (ino *inode) isDir() bool {
	return ino.mode&0xF000 == 0o040000
}

func (ino *inode) isSymlink() bool {
	return ino.mode&0xF000 == 0o120000
}

func (ino *inode) isRegular() bool {
	return ino.mode&0xF000 == 0o100000
}

func (ino *inode) isCompressed() bool {
	return ino.dataLayout == ondisk.InodeCompressedFull ||
		ino.dataLayout == ondisk.InodeCompressedCompact
}

// metaEnd returns the byte offset right after the inode metadata (inode + xattrs).
func (ino *inode) metaEnd() int64 {
	return ino.iloc + int64(ino.inodeSize) + int64(ino.xattrISize)
}

// fileInfo returns an fs.FileInfo for this inode.
func (ino *inode) fileInfo(name string) fs.FileInfo {
	return &fileInfo{
		name:    name,
		size:    int64(ino.size),
		mode:    modeFromErofs(ino.mode),
		modTime: ino.mtime,
	}
}

type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *fileInfo) Sys() any           { return nil }

func modeFromErofs(imode uint16) fs.FileMode {
	mode := fs.FileMode(imode & 0o7777)
	switch imode & 0xF000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	}
	return mode
}

// readInlineData reads inline data from the inode metadata area.
func (f *FS) readInlineData(ino *inode, buf []byte, off int64) (int, error) {
	metaEnd := ino.metaEnd()
	dataOff := metaEnd + off
	n, err := f.r.ReadAt(buf, dataOff)
	if err == io.EOF && n > 0 {
		return n, io.EOF
	}
	return n, err
}

// readSymlink reads the symlink target from an inode.
func (f *FS) readSymlink(ino *inode) (string, error) {
	if ino.size == 0 {
		return "", nil
	}
	if ino.size > ondisk.NameLen*4 { // reasonable limit
		return "", fmt.Errorf("erofs: symlink too large: %d bytes", ino.size)
	}

	target := make([]byte, ino.size)

	switch ino.dataLayout {
	case ondisk.InodeFlatInline:
		// Fast symlink: target is inline after inode metadata.
		if _, err := f.r.ReadAt(target, ino.metaEnd()); err != nil {
			return "", fmt.Errorf("erofs: reading inline symlink: %w", err)
		}
	case ondisk.InodeFlatPlain:
		// Target is in data blocks.
		pa := int64(ino.startBlk) << f.blockSzBits
		if _, err := f.r.ReadAt(target, pa); err != nil {
			return "", fmt.Errorf("erofs: reading symlink data: %w", err)
		}
	default:
		return "", fmt.Errorf("erofs: unsupported symlink layout %d", ino.dataLayout)
	}

	return string(target), nil
}
