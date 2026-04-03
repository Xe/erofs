package erofs

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/Xe/erofs/internal/ondisk"
)

// dir implements fs.ReadDirFile for EROFS directories.
type dir struct {
	fsys    *FS
	ino     *inode
	name    string
	entries []fs.DirEntry
	pos     int
	loaded  bool
	closed  bool
}

func (f *FS) openDir(ino *inode, name string) (fs.File, error) {
	return &dir{fsys: f, ino: ino, name: name}, nil
}

func (d *dir) Stat() (fs.FileInfo, error) {
	return d.ino.fileInfo(baseName(d.name)), nil
}

func (d *dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fmt.Errorf("is a directory")}
}

func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, fs.ErrClosed
	}
	if !d.loaded {
		entries, err := d.fsys.readDirEntries(d.ino)
		if err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: d.name, Err: err}
		}
		d.entries = entries
		d.loaded = true
	}

	if n <= 0 {
		if d.pos >= len(d.entries) {
			return nil, nil
		}
		result := d.entries[d.pos:]
		d.pos = len(d.entries)
		return result, nil
	}

	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}

	end := d.pos + n
	if end > len(d.entries) {
		end = len(d.entries)
	}
	result := d.entries[d.pos:end]
	d.pos = end
	if d.pos >= len(d.entries) {
		return result, io.EOF
	}
	return result, nil
}

func (d *dir) Close() error {
	d.closed = true
	return nil
}

// readDirEntries reads all directory entries from a directory inode.
func (f *FS) readDirEntries(ino *inode) ([]fs.DirEntry, error) {
	if ino.size == 0 {
		return nil, nil
	}

	dirData := make([]byte, ino.size)
	if err := f.readInodeData(ino, dirData); err != nil {
		return nil, err
	}

	blockSize := f.blockSize
	var entries []fs.DirEntry

	for blockStart := 0; blockStart < len(dirData); blockStart += blockSize {
		blockEnd := blockStart + blockSize
		if blockEnd > len(dirData) {
			blockEnd = len(dirData)
		}
		block := dirData[blockStart:blockEnd]

		if len(block) < ondisk.DirentSize {
			break
		}

		// First dirent's nameoff tells us where names start / how many entries.
		firstNameOff := int(binary.LittleEndian.Uint16(block[8:10])) & (blockSize - 1)
		if firstNameOff < ondisk.DirentSize || firstNameOff > len(block) {
			return nil, fmt.Errorf("erofs: invalid first nameoff %d in dir block", firstNameOff)
		}
		entryCount := firstNameOff / ondisk.DirentSize

		for i := 0; i < entryCount; i++ {
			deOff := i * ondisk.DirentSize
			if deOff+ondisk.DirentSize > len(block) {
				break
			}

			nid := binary.LittleEndian.Uint64(block[deOff:])
			nameOff := int(binary.LittleEndian.Uint16(block[deOff+8:])) & (blockSize - 1)
			fileType := block[deOff+10]

			// Determine name length.
			var nameLen int
			if i < entryCount-1 {
				nextNameOff := int(binary.LittleEndian.Uint16(block[(i+1)*ondisk.DirentSize+8:])) & (blockSize - 1)
				nameLen = nextNameOff - nameOff
			} else {
				// Last entry: scan for null terminator.
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
			// Trim any trailing nulls.
			name = strings.TrimRight(name, "\x00")
			if name == "" || name == "." || name == ".." {
				continue
			}

			nid &= ondisk.DirentNIDMask

			entries = append(entries, &dirEntry{
				name:     name,
				fileType: fileType,
				fsys:     f,
				nid:      nid,
			})
		}
	}

	return entries, nil
}

// lookupDir looks up a name in a directory, returning (nid, file_type, error).
// Uses binary search for efficient lookup.
func (f *FS) lookupDir(ino *inode, target string) (uint64, uint8, error) {
	if ino.size == 0 {
		return 0, 0, fs.ErrNotExist
	}

	dirData := make([]byte, ino.size)
	if err := f.readInodeData(ino, dirData); err != nil {
		return 0, 0, err
	}

	blockSize := f.blockSize
	numBlocks := (int(ino.size) + blockSize - 1) / blockSize

	// Level 1: binary search across blocks.
	head, back := 0, numBlocks-1
	candidateBlock := -1
	candidateNDirents := 0

	for head <= back {
		mid := head + (back-head)/2
		blockStart := mid * blockSize
		blockEnd := blockStart + blockSize
		if blockEnd > len(dirData) {
			blockEnd = len(dirData)
		}
		block := dirData[blockStart:blockEnd]

		if len(block) < ondisk.DirentSize {
			back = mid - 1
			continue
		}

		firstNameOff := int(binary.LittleEndian.Uint16(block[8:10])) & (blockSize - 1)
		if firstNameOff < ondisk.DirentSize || firstNameOff > len(block) {
			back = mid - 1
			continue
		}
		ndirents := firstNameOff / ondisk.DirentSize

		// Get first entry's name.
		var firstNameEnd int
		if ndirents > 1 {
			firstNameEnd = int(binary.LittleEndian.Uint16(block[ondisk.DirentSize+8:])) & (blockSize - 1)
		} else {
			firstNameEnd = len(block)
		}
		firstName := extractName(block, firstNameOff, firstNameEnd)

		cmp := strings.Compare(target, firstName)
		if cmp < 0 {
			back = mid - 1
		} else if cmp == 0 {
			// Exact match on first entry.
			nid := binary.LittleEndian.Uint64(block[0:]) & ondisk.DirentNIDMask
			ft := block[10]
			return nid, ft, nil
		} else {
			candidateBlock = mid
			candidateNDirents = ndirents
			head = mid + 1
		}
	}

	if candidateBlock < 0 {
		return 0, 0, fs.ErrNotExist
	}

	// Level 2: binary search within candidate block.
	blockStart := candidateBlock * blockSize
	blockEnd := blockStart + blockSize
	if blockEnd > len(dirData) {
		blockEnd = len(dirData)
	}
	block := dirData[blockStart:blockEnd]

	return searchBlock(block, blockSize, candidateNDirents, target)
}

// searchBlock does binary search within a directory block.
func searchBlock(block []byte, blockSize, ndirents int, target string) (uint64, uint8, error) {
	head, back := 0, ndirents-1
	for head <= back {
		mid := head + (back-head)/2

		deOff := mid * ondisk.DirentSize
		nameOff := int(binary.LittleEndian.Uint16(block[deOff+8:])) & (blockSize - 1)

		var nameEnd int
		if mid < ndirents-1 {
			nameEnd = int(binary.LittleEndian.Uint16(block[(mid+1)*ondisk.DirentSize+8:])) & (blockSize - 1)
		} else {
			nameEnd = len(block)
		}
		name := extractName(block, nameOff, nameEnd)

		cmp := strings.Compare(target, name)
		if cmp < 0 {
			back = mid - 1
		} else if cmp == 0 {
			nid := binary.LittleEndian.Uint64(block[deOff:]) & ondisk.DirentNIDMask
			ft := block[deOff+10]
			return nid, ft, nil
		} else {
			head = mid + 1
		}
	}
	return 0, 0, fs.ErrNotExist
}

// extractName gets a filename from a directory block.
func extractName(block []byte, start, end int) string {
	if start >= len(block) {
		return ""
	}
	if end > len(block) {
		end = len(block)
	}
	name := block[start:end]
	// Trim null terminators.
	for len(name) > 0 && name[len(name)-1] == 0 {
		name = name[:len(name)-1]
	}
	return string(name)
}

// readInodeData reads all data from an inode into dst.
func (f *FS) readInodeData(ino *inode, dst []byte) error {
	switch ino.dataLayout {
	case ondisk.InodeFlatPlain:
		pa := int64(ino.startBlk) << f.blockSzBits
		_, err := f.r.ReadAt(dst, pa)
		return err

	case ondisk.InodeFlatInline:
		blockSize := int64(f.blockSize)
		tailSize := int64(ino.size) % blockSize
		precedingSize := int64(ino.size) - tailSize

		if precedingSize > 0 {
			pa := int64(ino.startBlk) << f.blockSzBits
			if _, err := f.r.ReadAt(dst[:precedingSize], pa); err != nil {
				return err
			}
		}
		if tailSize > 0 {
			inlineOff := ino.metaEnd()
			if _, err := f.r.ReadAt(dst[precedingSize:], inlineOff); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("erofs: unsupported layout %d for bulk read", ino.dataLayout)
	}
}

// Lstat returns a FileInfo describing the named file without following symlinks.
func (f *FS) Lstat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrInvalid}
	}
	ino, err := f.resolveNoFollow(name)
	if err != nil {
		return nil, err
	}
	return ino.fileInfo(baseName(name)), nil
}
