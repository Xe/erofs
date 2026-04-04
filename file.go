package erofs

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/Xe/erofs/internal/ondisk"
)

// file implements fs.File, io.ReadSeeker, and io.ReaderAt for uncompressed files.
type file struct {
	fsys   *FS
	ino    *inode
	name   string
	offset int64
	closed bool
}

func (f *FS) openFile(ino *inode, name string) (fs.File, error) {
	if ino.isCompressed() {
		return f.openCompressedFile(ino, name)
	}
	return &file{fsys: f, ino: ino, name: name}, nil
}

func (fi *file) Stat() (fs.FileInfo, error) {
	return fi.ino.fileInfo(baseName(fi.name)), nil
}

func (fi *file) Read(p []byte) (int, error) {
	if fi.closed {
		return 0, fs.ErrClosed
	}
	if fi.offset >= int64(fi.ino.size) {
		return 0, io.EOF
	}

	n, err := fi.ReadAt(p, fi.offset)
	fi.offset += int64(n)
	return n, err
}

func (fi *file) ReadAt(p []byte, off int64) (int, error) {
	if fi.closed {
		return 0, fs.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("erofs: negative offset")
	}
	if off >= int64(fi.ino.size) {
		return 0, io.EOF
	}

	// Clamp read to file size.
	remaining := int64(fi.ino.size) - off
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	total := 0
	for len(p) > 0 {
		n, err := fi.readChunk(p, off)
		total += n
		off += int64(n)
		p = p[n:]
		if err != nil {
			if err == io.EOF && total > 0 {
				return total, io.EOF
			}
			return total, err
		}
	}

	if off >= int64(fi.ino.size) {
		return total, io.EOF
	}
	return total, nil
}

func (fi *file) Seek(offset int64, whence int) (int64, error) {
	if fi.closed {
		return 0, fs.ErrClosed
	}
	var newOff int64
	switch whence {
	case io.SeekStart:
		newOff = offset
	case io.SeekCurrent:
		newOff = fi.offset + offset
	case io.SeekEnd:
		newOff = int64(fi.ino.size) + offset
	default:
		return 0, fmt.Errorf("erofs: invalid whence %d", whence)
	}
	if newOff < 0 {
		return 0, fmt.Errorf("erofs: negative seek position")
	}
	fi.offset = newOff
	return newOff, nil
}

func (fi *file) Close() error {
	fi.closed = true
	return nil
}

// readChunk reads a contiguous chunk of data at the given offset.
func (fi *file) readChunk(p []byte, off int64) (int, error) {
	ino := fi.ino
	blockSize := int64(fi.fsys.blockSize)

	switch ino.dataLayout {
	case ondisk.InodeFlatPlain:
		pa := int64(ino.startBlk)<<fi.fsys.blockSzBits + off
		return fi.fsys.r.ReadAt(p, pa)

	case ondisk.InodeFlatInline:
		tailSize := int64(ino.size) % blockSize
		precedingSize := int64(ino.size) - tailSize

		if off < precedingSize {
			// In preceding blocks.
			pa := int64(ino.startBlk)<<fi.fsys.blockSzBits + off
			// Don't read past the preceding region.
			maxLen := precedingSize - off
			if int64(len(p)) > maxLen {
				p = p[:maxLen]
			}
			return fi.fsys.r.ReadAt(p, pa)
		}

		// In inline tail.
		inlineOff := ino.metaEnd() + (off - precedingSize)
		maxLen := int64(ino.size) - off
		if int64(len(p)) > maxLen {
			p = p[:maxLen]
		}
		return fi.fsys.r.ReadAt(p, inlineOff)

	case ondisk.InodeChunkBased:
		return fi.readChunkBased(p, off)

	default:
		return 0, fmt.Errorf("erofs: unsupported data layout %d for read", ino.dataLayout)
	}
}

// readChunkBased reads from a chunk-based inode.
func (fi *file) readChunkBased(p []byte, off int64) (int, error) {
	ino := fi.ino
	chunkSize := int64(1) << ino.chunkBits
	chunkNum := off >> ino.chunkBits
	chunkOff := off & (chunkSize - 1)

	// Don't read past chunk boundary.
	maxLen := chunkSize - chunkOff
	if int64(len(p)) > maxLen {
		p = p[:maxLen]
	}

	// Read chunk entry.
	var entrySize int64
	if ino.chunkFormat&ondisk.ChunkFormatIndexes != 0 {
		entrySize = 8
	} else {
		entrySize = 4
	}

	entryStart := align(ino.metaEnd(), entrySize)
	entryOff := entryStart + entrySize*chunkNum

	entryBuf := make([]byte, entrySize)
	if _, err := fi.fsys.r.ReadAt(entryBuf, entryOff); err != nil {
		return 0, fmt.Errorf("erofs: reading chunk entry: %w", err)
	}

	var blkAddr uint64
	var deviceID uint16
	if entrySize == 8 {
		idx := ondisk.ChunkIndex{
			StartBlkHi: le16(entryBuf[0:2]),
			DeviceID:   le16(entryBuf[2:4]),
			StartBlkLo: le32(entryBuf[4:8]),
		}
		blkAddr = uint64(idx.StartBlkLo) | uint64(idx.StartBlkHi)<<32
		deviceID = idx.DeviceID & fi.fsys.deviceIDMask
	} else {
		blkAddr = uint64(le32(entryBuf[0:4]))
	}

	// Check for hole. NullAddr is 0xFFFFFFFF for 32-bit entries.
	// For 8-byte indexed entries, check based on address width.
	isHole := false
	if entrySize == 4 {
		isHole = uint32(blkAddr) == uint32(ondisk.NullAddr)
	} else {
		if ino.chunkFormat&ondisk.ChunkFormat48Bit != 0 {
			isHole = blkAddr&0xFFFFFFFFFFFF == 0xFFFFFFFFFFFF
		} else {
			isHole = uint32(blkAddr) == uint32(ondisk.NullAddr)
		}
	}
	if isHole {
		// Hole - return zeros.
		clear(p)
		return len(p), nil
	}

	pa := int64(blkAddr)<<fi.fsys.blockSzBits + chunkOff

	// In flat device mode, adjust by the device's unified address.
	if fi.fsys.flatDev && deviceID > 0 {
		idx := int(deviceID) - 1
		if idx < len(fi.fsys.devices) {
			pa += int64(fi.fsys.devices[idx].UniAddr) << fi.fsys.blockSzBits
		}
		return fi.fsys.r.ReadAt(p, pa)
	}

	return fi.fsys.readerForDevice(deviceID).ReadAt(p, pa)
}

func baseName(name string) string {
	if name == "." {
		return "."
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return name
}

func align(v, a int64) int64 {
	return (v + a - 1) &^ (a - 1)
}

func le16(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
