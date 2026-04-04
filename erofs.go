// Package erofs implements a read-only EROFS (Enhanced Read-Only File System)
// reader as a Go fs.FS.
//
// EROFS is a Linux kernel filesystem designed for read-only use cases such as
// container images, system partitions, and live media.
//
// Usage:
//
//	f, err := os.Open("image.erofs")
//	if err != nil { ... }
//	defer f.Close()
//
//	fsys, err := erofs.Open(f)
//	if err != nil { ... }
//
//	data, err := fs.ReadFile(fsys, "etc/passwd")
package erofs

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
)

// FS is an EROFS filesystem image opened for reading.
// It implements fs.FS, fs.StatFS, and fs.ReadLinkFS.
type FS struct {
	r           io.ReaderAt
	blockSize   int
	blockSzBits uint8
	metaBlkOff  int64 // meta_blkaddr * block_size
	rootNID     uint64
	epoch       int64
	fixedNsec   uint32
	sb          *ondisk.SuperBlock

	devices      []DeviceInfo  // parsed device table (len == sb.ExtraDevices)
	blobs        []io.ReaderAt // external blob readers (one per extra device)
	deviceIDMask uint16        // bitmask for valid device IDs
	flatDev      bool          // flat addressing mode
}

// Open opens an EROFS filesystem image from the given io.ReaderAt.
func Open(r io.ReaderAt) (*FS, error) {
	sb, err := readSuperBlock(r)
	if err != nil {
		return nil, err
	}

	blockSize := 1 << sb.BlkSzBits

	f := &FS{
		r:           r,
		blockSize:   blockSize,
		blockSzBits: sb.BlkSzBits,
		metaBlkOff:  int64(sb.MetaBlkAddr) * int64(blockSize),
		epoch:       sb.Epoch,
		fixedNsec:   sb.FixedNsec,
		sb:          sb,
	}

	// Resolve root NID.
	if sb.FeatureIncompat&ondisk.FeatureIncompat48Bit != 0 {
		f.rootNID = sb.RootNID8B
	} else {
		f.rootNID = uint64(sb.RootNID2B)
	}

	// Verify checksum.
	if err := verifySuperBlockChecksum(r, sb); err != nil {
		return nil, err
	}

	// Validate root inode exists and is a directory.
	root, err := f.readInode(f.rootNID)
	if err != nil {
		return nil, fmt.Errorf("erofs: reading root inode: %w", err)
	}
	if !root.isDir() {
		return nil, fmt.Errorf("erofs: root inode is not a directory")
	}

	// Parse device table if present.
	if sb.ExtraDevices > 0 {
		devtOff := int64(sb.DevtSlotOff) * int64(ondisk.DevTSlotSize)
		devs, err := parseDeviceTable(r, sb.ExtraDevices, devtOff)
		if err != nil {
			return nil, err
		}
		f.devices = devs
		f.deviceIDMask = computeDeviceIDMask(sb.ExtraDevices)
		f.flatDev = true // default to flat; OpenMultiBlob sets to false
	}

	return f, nil
}

// OpenMultiBlob opens a multi-blob EROFS image. The primary image is read
// from r. Each extra device listed in the superblock's device table is
// backed by the corresponding entry in blobs. The length of blobs must
// equal the superblock's ExtraDevices count.
//
// For images with no extra devices, use Open() instead.
func OpenMultiBlob(r io.ReaderAt, blobs []io.ReaderAt) (*FS, error) {
	f, err := Open(r)
	if err != nil {
		return nil, err
	}

	if len(blobs) != len(f.devices) {
		return nil, fmt.Errorf("erofs: blob count mismatch: got %d, image has %d extra devices",
			len(blobs), len(f.devices))
	}

	f.blobs = blobs
	f.flatDev = false
	return f, nil
}

// readerForDevice returns the io.ReaderAt for the given device ID.
// Device ID 0 is the primary image. Device IDs 1..N map to blobs[0..N-1].
func (f *FS) readerForDevice(deviceID uint16) io.ReaderAt {
	if deviceID == 0 || f.blobs == nil {
		return f.r
	}
	idx := int(deviceID) - 1
	if idx >= len(f.blobs) {
		return f.r // fallback; shouldn't happen with valid images
	}
	return f.blobs[idx]
}

// Open opens the named file.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	ino, err := f.resolve(name)
	if err != nil {
		return nil, err
	}

	if ino.isDir() {
		return f.openDir(ino, name)
	}
	return f.openFile(ino, name)
}

// Stat returns a FileInfo describing the named file.
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	ino, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	return ino.fileInfo(path.Base(name)), nil
}

// ReadLink returns the destination of the named symbolic link.
func (f *FS) ReadLink(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	ino, err := f.resolveNoFollow(name)
	if err != nil {
		return "", err
	}
	if !ino.isSymlink() {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fmt.Errorf("not a symlink")}
	}
	return f.readSymlink(ino)
}

// resolve resolves a path to an inode, following symlinks.
func (f *FS) resolve(name string) (*inode, error) {
	return f.resolveAt(name, 0, true)
}

// resolveNoFollow resolves a path without following the final symlink component.
func (f *FS) resolveNoFollow(name string) (*inode, error) {
	return f.resolveAt(name, 0, false)
}

func (f *FS) resolveAt(name string, depth int, followFinal bool) (*inode, error) {
	if depth > 40 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("too many symlinks")}
	}

	root, err := f.readInode(f.rootNID)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if name == "." {
		return root, nil
	}

	current := root
	parts := strings.Split(name, "/")

	for i, part := range parts {
		if !current.isDir() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("not a directory")}
		}

		nid, _, err := f.lookupDir(current, part)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}

		next, err := f.readInode(nid)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}

		isLast := i == len(parts)-1
		if next.isSymlink() && (followFinal || !isLast) {
			target, err := f.readSymlink(next)
			if err != nil {
				return nil, &fs.PathError{Op: "open", Path: name, Err: err}
			}

			var resolved string
			if path.IsAbs(target) {
				// Absolute symlink: resolve from root, stripping leading /.
				resolved = strings.TrimPrefix(target, "/")
			} else {
				// Relative symlink: resolve from parent directory.
				parent := strings.Join(parts[:i], "/")
				if parent == "" {
					resolved = target
				} else {
					resolved = parent + "/" + target
				}
			}

			// Append remaining path components.
			if !isLast {
				remaining := strings.Join(parts[i+1:], "/")
				resolved = resolved + "/" + remaining
			}

			resolved = path.Clean(resolved)
			if !fs.ValidPath(resolved) {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
			}

			return f.resolveAt(resolved, depth+1, followFinal)
		}

		current = next
	}

	return current, nil
}

// Compile-time interface checks.
var (
	_ fs.FS         = (*FS)(nil)
	_ fs.StatFS     = (*FS)(nil)
	_ fs.ReadLinkFS = (*FS)(nil)
)

// dirEntry implements fs.DirEntry.
type dirEntry struct {
	name     string
	fileType uint8
	ino      *inode
	fsys     *FS
	nid      uint64
}

func (d *dirEntry) Name() string { return d.name }
func (d *dirEntry) IsDir() bool  { return d.fileType == ondisk.FTDir }
func (d *dirEntry) Type() fs.FileMode {
	switch d.fileType {
	case ondisk.FTDir:
		return fs.ModeDir
	case ondisk.FTSymlink:
		return fs.ModeSymlink
	case ondisk.FTChrDev:
		return fs.ModeDevice | fs.ModeCharDevice
	case ondisk.FTBlkDev:
		return fs.ModeDevice
	case ondisk.FTFIFO:
		return fs.ModeNamedPipe
	case ondisk.FTSock:
		return fs.ModeSocket
	default:
		return 0
	}
}

func (d *dirEntry) Info() (fs.FileInfo, error) {
	if d.ino == nil {
		ino, err := d.fsys.readInode(d.nid)
		if err != nil {
			return nil, err
		}
		d.ino = ino
	}
	return d.ino.fileInfo(d.name), nil
}

// modTimeFromSB returns a zero time - used when no mtime info available.
func modTimeFromSB(sb *ondisk.SuperBlock) time.Time {
	return time.Unix(sb.Epoch+int64(sb.BuildTime), 0)
}
