package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"

	"github.com/Xe/erofs/internal/ondisk"
)

// ChunkRef describes one chunk of a file, pointing at a block address on a
// specific device.
type ChunkRef struct {
	DeviceID uint16 // 0 = primary image, 1+ = blob index+1
	BlkAddr  uint64 // starting block address within the device
	Size     uint32 // chunk data size in bytes (only last chunk may be < chunkSize)
}

// BlobInfo describes an external blob device to include in the device table.
type BlobInfo struct {
	Tag    [64]byte // identifier (e.g. content digest)
	Blocks uint64   // total blocks in this blob
}

// WithFlatDevice enables flat device mode. In flat mode, all blob data is
// mapped into a single unified address space. The builder computes UniAddr
// offsets for each device so that a composed block device can be mounted
// directly.
func WithFlatDevice() BuildOption {
	return func(b *Builder) {
		b.flatDev = true
	}
}

// WithChunkSize sets the chunk size as a power of two bit count (e.g., 12 for
// 4096-byte chunks). Enables chunk-based inode layout for files added via
// AddChunkedFile.
func WithChunkSize(bits uint8) BuildOption {
	return func(b *Builder) {
		b.chunkBits = bits
	}
}

// AddChunkedFile adds a file whose data is stored externally in blob devices.
// The chunks slice describes the location of each sequential chunk. Each chunk
// is chunkSize bytes (set via WithChunkSize), except the last which may be smaller.
func (b *Builder) AddChunkedFile(p string, info fs.FileInfo, chunks []ChunkRef) error {
	p = cleanPath(p)
	if p == "/" {
		return fmt.Errorf("erofs: cannot add chunked file at root")
	}
	if b.chunkBits == 0 {
		return fmt.Errorf("erofs: chunk size not set; use WithChunkSize")
	}
	if b.chunkBits < b.blkSzBits {
		return fmt.Errorf("erofs: chunk size bits (%d) must be >= block size bits (%d)", b.chunkBits, b.blkSzBits)
	}

	ino := &buildInode{
		path:       p,
		mode:       info.Mode(),
		size:       info.Size(),
		mtime:      info.ModTime(),
		dataLayout: ondisk.InodeChunkBased,
		chunks:     chunks,
	}

	b.inodes = append(b.inodes, ino)
	b.addToParent(p, ino)

	// Track max device ID for device table sizing.
	for _, c := range chunks {
		if c.DeviceID > b.maxDeviceID {
			b.maxDeviceID = c.DeviceID
		}
	}

	return nil
}

// SetBlobInfo sets the metadata for an external blob device at index (1-based
// device ID). Must be called before Build() for each blob referenced by
// AddChunkedFile.
func (b *Builder) SetBlobInfo(deviceID uint16, info BlobInfo) {
	if b.blobInfos == nil {
		b.blobInfos = make(map[uint16]BlobInfo)
	}
	b.blobInfos[deviceID] = info
}

// writeChunkedInode writes a chunk-based inode and its chunk index entries.
func (b *Builder) writeChunkedInode(ino *buildInode) error {
	chunkFormat := uint16(ondisk.ChunkFormatIndexes)
	if b.chunkBits > b.blkSzBits {
		chunkFormat |= uint16(b.chunkBits-b.blkSzBits) & ondisk.ChunkFormatBlkBitsMask
	}

	mtSec, _ := b.diskMtime(ino.mtime)
	ei := ondisk.InodeExtended{
		Format: uint16(ondisk.InodeLayoutExtended) | uint16(ino.dataLayout)<<ondisk.IDataLayoutBit,
		Mode:   erofsModeFromFS(ino.mode),
		Size:   uint64(ino.size),
		U:      uint32(chunkFormat),
		UID:    ino.uid,
		GID:    ino.gid,
		Mtime:  mtSec,
		NLink:  1,
		NB:     0, // extended inode: offset-6 is startblk_hi/blocks_hi, not nlink
		Ino:    uint32(ino.nid),
	}

	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, &ei)
	inodeBytes := buf.Bytes()

	if _, err := b.w.WriteAt(inodeBytes, ino.metaOff); err != nil {
		return fmt.Errorf("erofs: writing chunked inode %s: %w", ino.path, err)
	}

	// Write chunk index entries (8 bytes each) after the inode, aligned to 8.
	idxOff := alignUp(ino.metaOff+64, 8)
	for _, c := range ino.chunks {
		var entry [8]byte
		binary.LittleEndian.PutUint16(entry[0:2], uint16(c.BlkAddr>>32))
		binary.LittleEndian.PutUint16(entry[2:4], c.DeviceID)
		binary.LittleEndian.PutUint32(entry[4:8], uint32(c.BlkAddr))
		if _, err := b.w.WriteAt(entry[:], idxOff); err != nil {
			return fmt.Errorf("erofs: writing chunk index for %s: %w", ino.path, err)
		}
		idxOff += 8
	}
	return nil
}
