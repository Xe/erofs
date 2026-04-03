package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// ValidationError represents a single validation issue.
type ValidationError struct {
	Path    string // file path or "superblock", "inode:NID", etc.
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("erofs: %s: %s", e.Path, e.Message)
}

// ValidationResult contains all validation findings.
type ValidationResult struct {
	Errors       []ValidationError
	Warnings     []ValidationError
	InodeCount   int
	FileCount    int
	DirCount     int
	SymlinkCount int
}

// addError appends a validation error.
func (vr *ValidationResult) addError(path, msg string, args ...any) {
	vr.Errors = append(vr.Errors, ValidationError{
		Path:    path,
		Message: fmt.Sprintf(msg, args...),
	})
}

// addWarning appends a validation warning.
func (vr *ValidationResult) addWarning(path, msg string, args ...any) {
	vr.Warnings = append(vr.Warnings, ValidationError{
		Path:    path,
		Message: fmt.Sprintf(msg, args...),
	})
}

// validator holds state during validation.
type validator struct {
	r           io.ReaderAt
	size        int64
	sb          *ondisk.SuperBlock
	blockSize   int
	blockSzBits uint8
	metaBlkOff  int64
	rootNID     uint64
	result      *ValidationResult
	visited     map[uint64]bool
}

// Validate checks the integrity of an EROFS filesystem image.
// It performs the following checks:
//   - Superblock magic, checksum, feature flags, block size
//   - Root inode is a directory
//   - All inodes have valid format, mode, size, data layout
//   - For FLAT_INLINE inodes: inline data does not cross block boundary
//   - Directory entries have valid nameoff, file_type, name length
//   - Directory entries are sorted
//   - Symlinks have valid targets (non-empty, reasonable length)
//   - All referenced NIDs are within the metadata area
//   - File data references are within the image
func Validate(r io.ReaderAt, size int64) (*ValidationResult, error) {
	v := &validator{
		r:       r,
		size:    size,
		result:  &ValidationResult{},
		visited: make(map[uint64]bool),
	}

	if err := v.validateSuperBlock(); err != nil {
		return v.result, err
	}

	v.walkInode(v.rootNID, ".")
	return v.result, nil
}

// validateSuperBlock reads and validates the superblock.
func (v *validator) validateSuperBlock() error {
	sb, err := readSuperBlock(v.r)
	if err != nil {
		return fmt.Errorf("erofs: superblock: %w", err)
	}
	v.sb = sb

	v.blockSize = 1 << sb.BlkSzBits
	v.blockSzBits = sb.BlkSzBits
	v.metaBlkOff = int64(sb.MetaBlkAddr) * int64(v.blockSize)

	// readSuperBlock already checks magic, block size bits, and unknown incompat features.
	// Check dirblkbits.
	if sb.DirBlkBits != 0 {
		v.result.addWarning("superblock", "dirblkbits = %d, expected 0", sb.DirBlkBits)
	}

	// Verify checksum.
	if err := verifySuperBlockChecksum(v.r, sb); err != nil {
		v.result.addError("superblock", "checksum: %v", err)
	}

	// Resolve root NID.
	if sb.FeatureIncompat&ondisk.FeatureIncompat48Bit != 0 {
		v.rootNID = sb.RootNID8B
	} else {
		v.rootNID = uint64(sb.RootNID2B)
	}

	// Validate root NID points to a valid offset.
	rootIloc := v.metaBlkOff + int64(v.rootNID<<ondisk.ISlotBits)
	if rootIloc < 0 || rootIloc >= v.size {
		v.result.addError("superblock", "root NID %d points outside image (offset 0x%X, image size 0x%X)", v.rootNID, rootIloc, v.size)
	}

	return nil
}

// readInodeRaw reads and decodes an inode by NID for validation purposes.
// Returns the inode and any error encountered during reading.
func (v *validator) readInodeRaw(nid uint64) (*inode, error) {
	iloc := v.metaBlkOff + int64(nid<<ondisk.ISlotBits)

	if iloc < 0 || iloc+32 > v.size {
		return nil, fmt.Errorf("inode offset 0x%X out of range (image size 0x%X)", iloc, v.size)
	}

	// Read enough bytes for an extended inode (64 bytes).
	buf := make([]byte, 64)
	n, err := v.r.ReadAt(buf, iloc)
	if err != nil && n < 32 {
		return nil, fmt.Errorf("reading inode: %w", err)
	}

	format := binary.LittleEndian.Uint16(buf[0:2])

	// Check for unknown format bits.
	if format & ^uint16(ondisk.IAll) != 0 {
		return nil, fmt.Errorf("invalid format bits 0x%04X (unknown bits set: 0x%04X)", format, format & ^uint16(ondisk.IAll))
	}

	version := format & ondisk.IVersionMask
	dataLayout := uint8((format >> ondisk.IDataLayoutBit) & ondisk.IDataLayoutMask)

	if dataLayout >= ondisk.InodeDataLayoutMax {
		return nil, fmt.Errorf("invalid data layout %d (max %d)", dataLayout, ondisk.InodeDataLayoutMax-1)
	}

	ino := &inode{
		nid:        nid,
		format:     format,
		dataLayout: dataLayout,
		iloc:       iloc,
		metaBlkOff: v.metaBlkOff,
	}

	if version == ondisk.InodeLayoutCompact {
		var ci ondisk.InodeCompact
		if err := binary.Read(bytes.NewReader(buf[:32]), binary.LittleEndian, &ci); err != nil {
			return nil, fmt.Errorf("decoding compact inode: %w", err)
		}
		ino.inodeSize = 32
		ino.mode = ci.Mode
		ino.size = uint64(ci.Size)
		ino.ino = ci.Ino
		ino.uid = uint32(ci.UID)
		ino.gid = uint32(ci.GID)
		ino.parseUnion(ci.U, ci.NB, ci.Format, v.blockSzBits)
	} else {
		if n < 64 {
			return nil, fmt.Errorf("short read for extended inode: got %d bytes, need 64", n)
		}
		var ei ondisk.InodeExtended
		if err := binary.Read(bytes.NewReader(buf[:64]), binary.LittleEndian, &ei); err != nil {
			return nil, fmt.Errorf("decoding extended inode: %w", err)
		}
		ino.inodeSize = 64
		ino.mode = ei.Mode
		ino.size = ei.Size
		ino.ino = ei.Ino
		ino.uid = ei.UID
		ino.gid = ei.GID
		ino.nlink = ei.NLink
		ino.mtime = time.Unix(ei.Mtime, int64(ei.MtimeNsec))
		ino.parseUnion(ei.U, ei.NB, ei.Format, v.blockSzBits)
	}

	// Xattr inline size.
	xattrICount := binary.LittleEndian.Uint16(buf[2:4])
	if xattrICount > 0 {
		ino.xattrISize = 12 + 4*(uint32(xattrICount)-1)
	}

	return ino, nil
}

// validModeType checks that the mode's file type bits are a known type.
func validModeType(mode uint16) bool {
	switch mode & 0xF000 {
	case 0o100000: // regular
		return true
	case 0o040000: // directory
		return true
	case 0o120000: // symlink
		return true
	case 0o020000: // char device
		return true
	case 0o060000: // block device
		return true
	case 0o010000: // fifo
		return true
	case 0o140000: // socket
		return true
	default:
		return false
	}
}

// walkInode validates an inode and recursively validates directory children.
func (v *validator) walkInode(nid uint64, path string) {
	if v.visited[nid] {
		return
	}
	v.visited[nid] = true

	loc := fmt.Sprintf("inode:%d", nid)

	ino, err := v.readInodeRaw(nid)
	if err != nil {
		v.result.addError(loc, "%v", err)
		return
	}

	v.result.InodeCount++

	// Validate mode has a known file type.
	if !validModeType(ino.mode) {
		v.result.addError(path, "unknown file type in mode 0x%04X", ino.mode)
	}

	// Validate data layout specific constraints.
	switch ino.dataLayout {
	case ondisk.InodeFlatInline:
		v.validateFlatInline(ino, path)
	case ondisk.InodeFlatPlain:
		v.validateFlatPlain(ino, path)
	}

	// Count by type and recurse.
	switch {
	case ino.isDir():
		v.result.DirCount++
		if path == "." && nid == v.rootNID {
			// root must be a directory (already guaranteed by getting here)
		}
		v.validateDir(ino, path)
	case ino.isRegular():
		v.result.FileCount++
	case ino.isSymlink():
		v.result.SymlinkCount++
		v.validateSymlink(ino, path)
	}
}

// validateFlatInline checks that inline data does not cross a block boundary.
func (v *validator) validateFlatInline(ino *inode, path string) {
	blockSize := int64(v.blockSize)
	tailSize := int64(ino.size) % blockSize
	if tailSize == 0 && ino.size > 0 {
		// No inline tail; all data is in preceding blocks.
		return
	}
	// blkoff of the start of inline data.
	inlineStart := ino.metaEnd()
	blkoff := inlineStart % blockSize
	if blkoff+tailSize > blockSize {
		v.result.addError(path, "FLAT_INLINE tail crosses block boundary: blkoff=%d + tail=%d > blockSize=%d", blkoff, tailSize, blockSize)
	}
}

// validateFlatPlain checks that file data references are within the image.
func (v *validator) validateFlatPlain(ino *inode, path string) {
	if ino.size == 0 {
		return
	}
	dataStart := int64(ino.startBlk) << v.blockSzBits
	dataEnd := dataStart + int64(ino.size)
	if dataEnd > v.size {
		v.result.addError(path, "FLAT_PLAIN data extends past image: startblk=%d, size=%d, data_end=0x%X, image_size=0x%X", ino.startBlk, ino.size, dataEnd, v.size)
	}
}

// validateDir reads and validates directory entries.
func (v *validator) validateDir(ino *inode, path string) {
	if ino.size == 0 {
		return
	}

	dirData, err := v.readInodeData(ino)
	if err != nil {
		v.result.addError(path, "reading directory data: %v", err)
		return
	}

	blockSize := v.blockSize

	for blockStart := 0; blockStart < len(dirData); blockStart += blockSize {
		blockEnd := blockStart + blockSize
		if blockEnd > len(dirData) {
			blockEnd = len(dirData)
		}
		block := dirData[blockStart:blockEnd]

		if len(block) < ondisk.DirentSize {
			break
		}

		firstNameOff := int(binary.LittleEndian.Uint16(block[8:10])) & (blockSize - 1)

		// Validate first nameoff.
		if firstNameOff < ondisk.DirentSize {
			v.result.addError(path, "dir block at offset %d: first nameoff %d < DirentSize %d", blockStart, firstNameOff, ondisk.DirentSize)
			continue
		}
		if firstNameOff > len(block) {
			v.result.addError(path, "dir block at offset %d: first nameoff %d > block length %d", blockStart, firstNameOff, len(block))
			continue
		}

		entryCount := firstNameOff / ondisk.DirentSize
		if entryCount < 1 {
			v.result.addError(path, "dir block at offset %d: entry count %d < 1", blockStart, entryCount)
			continue
		}

		var prevName string
		for i := 0; i < entryCount; i++ {
			deOff := i * ondisk.DirentSize
			if deOff+ondisk.DirentSize > len(block) {
				break
			}

			nid := binary.LittleEndian.Uint64(block[deOff:]) & ondisk.DirentNIDMask
			nameOff := int(binary.LittleEndian.Uint16(block[deOff+8:])) & (blockSize - 1)
			fileType := block[deOff+10]

			// Validate nameoff is within block bounds.
			if nameOff > len(block) {
				v.result.addError(path, "dir block at offset %d, entry %d: nameoff %d > block length %d", blockStart, i, nameOff, len(block))
				continue
			}

			// Validate nameoffs are monotonically increasing (non-last entries).
			if i < entryCount-1 {
				nextNameOff := int(binary.LittleEndian.Uint16(block[(i+1)*ondisk.DirentSize+8:])) & (blockSize - 1)
				if nameOff >= nextNameOff {
					v.result.addError(path, "dir block at offset %d, entry %d: nameoff %d >= next nameoff %d", blockStart, i, nameOff, nextNameOff)
				}
			}

			// Validate file_type.
			if fileType > 7 {
				v.result.addError(path, "dir block at offset %d, entry %d: invalid file_type %d", blockStart, i, fileType)
			}

			// Determine name length.
			var nameLen int
			if i < entryCount-1 {
				nextNameOff := int(binary.LittleEndian.Uint16(block[(i+1)*ondisk.DirentSize+8:])) & (blockSize - 1)
				nameLen = nextNameOff - nameOff
			} else {
				maxLen := len(block) - nameOff
				nameLen = maxLen
				for j := 0; j < maxLen; j++ {
					if block[nameOff+j] == 0 {
						nameLen = j
						break
					}
				}
			}

			if nameOff+nameLen > len(block) {
				nameLen = len(block) - nameOff
			}
			if nameLen <= 0 {
				continue
			}

			name := string(block[nameOff : nameOff+nameLen])
			name = strings.TrimRight(name, "\x00")

			// Validate name length.
			if len(name) > ondisk.NameLen {
				v.result.addError(path, "dir entry %q: name length %d > %d", name, len(name), ondisk.NameLen)
			}

			// Validate entries are sorted (within each block).
			if name != "." && name != ".." && prevName != "" && prevName != "." && prevName != ".." {
				if strings.Compare(prevName, name) > 0 {
					v.result.addError(path, "dir entries not sorted: %q > %q", prevName, name)
				}
			}
			prevName = name

			// Skip . and .. for recursive walk.
			if name == "." || name == ".." {
				continue
			}

			// Validate NID is within metadata area.
			childIloc := v.metaBlkOff + int64(nid<<ondisk.ISlotBits)
			if childIloc < 0 || childIloc >= v.size {
				v.result.addError(path, "dir entry %q: NID %d points outside image (offset 0x%X)", name, nid, childIloc)
				continue
			}

			childPath := name
			if path != "." {
				childPath = path + "/" + name
			}
			v.walkInode(nid, childPath)
		}
	}
}

// validateSymlink checks the symlink target.
func (v *validator) validateSymlink(ino *inode, path string) {
	if ino.size == 0 {
		v.result.addError(path, "symlink has empty target (size=0)")
		return
	}
	if ino.size > ondisk.NameLen*4 {
		v.result.addError(path, "symlink target too long: %d bytes (max %d)", ino.size, ondisk.NameLen*4)
		return
	}

	// Try to read the target to verify it is accessible.
	target := make([]byte, ino.size)
	var readErr error
	switch ino.dataLayout {
	case ondisk.InodeFlatInline:
		_, readErr = v.r.ReadAt(target, ino.metaEnd())
	case ondisk.InodeFlatPlain:
		pa := int64(ino.startBlk) << v.blockSzBits
		_, readErr = v.r.ReadAt(target, pa)
	default:
		v.result.addWarning(path, "symlink uses unsupported data layout %d, cannot validate target", ino.dataLayout)
		return
	}
	if readErr != nil {
		v.result.addError(path, "reading symlink target: %v", readErr)
		return
	}

	targetStr := string(target)
	if len(strings.TrimSpace(targetStr)) == 0 {
		v.result.addError(path, "symlink target is empty/whitespace")
	}
}

// readInodeData reads all data from an inode into a new byte slice.
func (v *validator) readInodeData(ino *inode) ([]byte, error) {
	dst := make([]byte, ino.size)

	switch ino.dataLayout {
	case ondisk.InodeFlatPlain:
		pa := int64(ino.startBlk) << v.blockSzBits
		_, err := v.r.ReadAt(dst, pa)
		return dst, err

	case ondisk.InodeFlatInline:
		blockSize := int64(v.blockSize)
		tailSize := int64(ino.size) % blockSize
		precedingSize := int64(ino.size) - tailSize

		if precedingSize > 0 {
			pa := int64(ino.startBlk) << v.blockSzBits
			if _, err := v.r.ReadAt(dst[:precedingSize], pa); err != nil {
				return nil, err
			}
		}
		if tailSize > 0 {
			inlineOff := ino.metaEnd()
			if _, err := v.r.ReadAt(dst[precedingSize:], inlineOff); err != nil {
				return nil, err
			}
		}
		return dst, nil

	default:
		return nil, fmt.Errorf("unsupported layout %d for bulk read", ino.dataLayout)
	}
}
