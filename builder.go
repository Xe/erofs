package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// Builder creates EROFS filesystem images.
type Builder struct {
	w                io.WriterAt
	blockSize        int
	blkSzBits        uint8
	pclusterBits     uint8 // log2 of pcluster size; 0 = auto (blkSzBits+4)
	inodes           []*buildInode
	dirs             map[string]*buildDir
	nextNID          uint64
	metaBlkAddr      uint32
	epoch            int64
	epochSet         bool
	compression      CompressionAlgorithm
	compressEnabled  bool
	compressedData   map[*buildInode]*compressedFileData
	zstdEnc          *zstd.Encoder
	lz4HC            *lz4.CompressorHC
	compressionLevel int                 // numeric level (zstd 1..22 / lz4 1..9); 0 = default
	chunkBits        uint8               // 0 means chunk mode disabled
	maxDeviceID      uint16              // highest device ID seen in AddChunkedFile
	blobInfos        map[uint16]BlobInfo // device ID -> blob metadata
	flatDev          bool                // flat device mode: compute UniAddr for each device
	endDataBlk       int64               // first block past all out-of-line data (for BlocksLo)
}

type buildInode struct {
	nid        uint64
	path       string // clean path within image ("/" for root)
	mode       fs.FileMode
	uid, gid   uint32
	size       int64
	mtime      time.Time
	data       []byte // file content or symlink target
	dataLayout uint8
	startBlk   uint64
	metaOff    int64 // absolute byte offset where metadata is written
	metaSize   int   // total metadata bytes (inode + inline tail)
	children   []buildDirent
	chunks     []ChunkRef // chunk references (for InodeChunkBased layout)
}

type buildDirent struct {
	name     string
	nid      uint64
	fileType uint8
}

type buildDir struct {
	inode    *buildInode
	children map[string]*buildInode
}

// BuildOption configures a Builder.
type BuildOption func(*Builder)

// WithBlockSize sets the block size as a power of two (e.g., 12 for 4096).
func WithBlockSize(bits uint8) BuildOption {
	return func(b *Builder) {
		b.blkSzBits = bits
		b.blockSize = 1 << bits
	}
}

// WithPClusterSize sets the pcluster (compression unit) size as a power-of-two
// bit count, e.g. 16 for 64 KiB. One pcluster spans pclusterSize/blockSize
// logical lclusters and is compressed as a single unit, so its compressed
// output can occupy fewer physical blocks than its logical span (a "big
// pcluster"). Defaults to blockSize*16, floored at 2 lclusters and capped at
// 1 MiB.
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

// WithEpoch sets the filesystem epoch timestamp. When set, all inode
// modification times and the superblock build time are pinned to this
// timestamp, making the image byte-for-byte reproducible.
func WithEpoch(t time.Time) BuildOption {
	return func(b *Builder) {
		b.epoch = t.Unix()
		b.epochSet = true
	}
}

// NewBuilder creates a new EROFS image builder that writes to w.
func NewBuilder(w io.WriterAt, opts ...BuildOption) *Builder {
	b := &Builder{
		w:         w,
		blkSzBits: 12,
		blockSize: 4096,
		dirs:      make(map[string]*buildDir),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// AddFile adds a regular file to the image.
func (b *Builder) AddFile(p string, info fs.FileInfo, data []byte) error {
	p = cleanPath(p)
	if p == "/" {
		return fmt.Errorf("erofs: cannot add file at root")
	}

	ino := &buildInode{
		path:  p,
		mode:  info.Mode(),
		size:  int64(len(data)),
		mtime: info.ModTime(),
		data:  data,
	}

	b.inodes = append(b.inodes, ino)
	b.addToParent(p, ino)
	return nil
}

// AddDir adds a directory to the image.
func (b *Builder) AddDir(p string, info fs.FileInfo) error {
	p = cleanPath(p)

	ino := &buildInode{
		path:  p,
		mode:  info.Mode() | fs.ModeDir,
		mtime: info.ModTime(),
	}

	b.inodes = append(b.inodes, ino)
	b.dirs[p] = &buildDir{
		inode:    ino,
		children: make(map[string]*buildInode),
	}

	if p != "/" {
		b.addToParent(p, ino)
	}
	return nil
}

// AddSymlink adds a symbolic link to the image.
func (b *Builder) AddSymlink(p string, target string, info fs.FileInfo) error {
	p = cleanPath(p)
	if p == "/" {
		return fmt.Errorf("erofs: cannot add symlink at root")
	}

	ino := &buildInode{
		path:  p,
		mode:  info.Mode() | fs.ModeSymlink,
		size:  int64(len(target)),
		mtime: info.ModTime(),
		data:  []byte(target),
	}

	b.inodes = append(b.inodes, ino)
	b.addToParent(p, ino)
	return nil
}

// AddFromFS walks an fs.FS and adds all entries to the image.
func (b *Builder) AddFromFS(fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("erofs: stat %s: %w", p, err)
		}

		// Normalize path: fs.WalkDir uses "." for root.
		imgPath := "/" + p
		if p == "." {
			imgPath = "/"
		}

		switch {
		case d.IsDir():
			return b.AddDir(imgPath, info)

		case d.Type()&fs.ModeSymlink != 0:
			// Try to read symlink target.
			rlFS, ok := fsys.(fs.ReadLinkFS)
			if !ok {
				return fmt.Errorf("erofs: fs does not support ReadLink for %s", p)
			}
			target, err := rlFS.ReadLink(p)
			if err != nil {
				return fmt.Errorf("erofs: readlink %s: %w", p, err)
			}
			return b.AddSymlink(imgPath, target, info)

		case d.Type().IsRegular():
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return fmt.Errorf("erofs: read %s: %w", p, err)
			}
			return b.AddFile(imgPath, info, data)

		default:
			// Skip unsupported types (devices, pipes, sockets).
			return nil
		}
	})
}

// Build finalizes the image and writes it to the underlying writer.
func (b *Builder) Build() error {
	// Step 1: Ensure root directory exists.
	if _, ok := b.dirs["/"]; !ok {
		now := time.Unix(b.epoch, 0)
		ino := &buildInode{
			path:  "/",
			mode:  fs.ModeDir | 0o755,
			mtime: now,
		}
		b.inodes = append([]*buildInode{ino}, b.inodes...)
		b.dirs["/"] = &buildDir{
			inode:    ino,
			children: make(map[string]*buildInode),
		}
	}

	// Ensure parent directories exist for all inodes.
	b.ensureParentDirs()

	// Step 2: Build directory entries.
	if err := b.buildDirEntries(); err != nil {
		return err
	}

	// Step 3: Decide data layout and compute metadata sizes.
	b.computeLayouts()

	// Step 3b: Try compressing eligible files.
	b.compressedData = make(map[*buildInode]*compressedFileData)
	if b.compressEnabled {
		b.tryCompressInodes()
	}

	// Step 4: Assign NIDs.
	b.assignNIDs()

	// Update directory entry NIDs (now that they are assigned).
	b.updateDirentNIDs()

	// Repack directory data with final NIDs.
	if err := b.buildDirEntries(); err != nil {
		return err
	}

	// Step 5: Lay out data blocks.
	b.layoutDataBlocks()

	// Step 6: Write metadata.
	if err := b.writeMetadata(); err != nil {
		return err
	}

	// Step 7: Write data blocks.
	if err := b.writeDataBlocks(); err != nil {
		return err
	}

	// Write the compression-config area (zstd only) BEFORE the superblock,
	// because the superblock checksum covers the first block including this
	// region.
	if err := b.writeComprCfgs(); err != nil {
		return fmt.Errorf("erofs: writing compr cfgs: %w", err)
	}

	// Step 8: Write superblock.
	if err := b.writeSuperblock(); err != nil {
		return err
	}

	// Step 9: Zero-fill first 1024 bytes.
	zeros := make([]byte, ondisk.SuperOffset)
	if _, err := b.w.WriteAt(zeros, 0); err != nil {
		return fmt.Errorf("erofs: writing zero padding: %w", err)
	}

	return nil
}

// addToParent registers an inode as a child of its parent directory.
func (b *Builder) addToParent(p string, ino *buildInode) {
	parent := path.Dir(p)
	if parent == "." {
		parent = "/"
	}

	d, ok := b.dirs[parent]
	if !ok {
		// Parent directory will be created later by ensureParentDirs.
		d = &buildDir{children: make(map[string]*buildInode)}
		b.dirs[parent] = d
	}
	d.children[path.Base(p)] = ino
}

// ensureParentDirs creates any missing intermediate directories.
func (b *Builder) ensureParentDirs() {
	for {
		added := false
		for p := range b.dirs {
			if p == "/" {
				continue
			}
			parent := path.Dir(p)
			if parent == "." {
				parent = "/"
			}
			if _, ok := b.dirs[parent]; !ok {
				now := time.Unix(b.epoch, 0)
				ino := &buildInode{
					path:  parent,
					mode:  fs.ModeDir | 0o755,
					mtime: now,
				}
				b.inodes = append(b.inodes, ino)
				b.dirs[parent] = &buildDir{
					inode:    ino,
					children: make(map[string]*buildInode),
				}
				b.addToParent(parent, ino)
				added = true
			}
		}
		if !added {
			break
		}
	}

	// Ensure all dirs have their inode set.
	for p, d := range b.dirs {
		if d.inode == nil {
			for _, ino := range b.inodes {
				if ino.path == p {
					d.inode = ino
					break
				}
			}
		}
	}
}

// buildDirEntries packs directory entries for each directory inode.
func (b *Builder) buildDirEntries() error {
	for dirPath, d := range b.dirs {
		if d.inode == nil {
			continue
		}

		// Collect children names sorted lexicographically.
		var names []string
		for name := range d.children {
			names = append(names, name)
		}
		sort.Strings(names)

		// Build the dirent list: "." and ".." first, then children.
		var dirents []buildDirent

		// "." entry
		dirents = append(dirents, buildDirent{
			name:     ".",
			nid:      d.inode.nid,
			fileType: ondisk.FTDir,
		})

		// ".." entry
		parentPath := path.Dir(dirPath)
		if parentPath == "." {
			parentPath = "/"
		}
		parentNID := d.inode.nid // root ".." points to self
		if dirPath != "/" {
			if pd, ok := b.dirs[parentPath]; ok && pd.inode != nil {
				parentNID = pd.inode.nid
			}
		}
		dirents = append(dirents, buildDirent{
			name:     "..",
			nid:      parentNID,
			fileType: ondisk.FTDir,
		})

		// Child entries.
		for _, name := range names {
			child := d.children[name]
			dirents = append(dirents, buildDirent{
				name:     name,
				nid:      child.nid,
				fileType: fileTypeFromMode(child.mode),
			})
		}

		d.inode.children = dirents

		// Pack directory data.
		dirData := b.packDirBlock(dirents)
		d.inode.data = dirData
		d.inode.size = int64(len(dirData))
	}
	return nil
}

// packDirBlock packs directory entries into EROFS directory blocks.
// Entries are grouped per block. Each block has an array of 12-byte dirent
// headers followed by the corresponding filenames. The last filename in
// each block is null-terminated.
func (b *Builder) packDirBlock(dirents []buildDirent) []byte {
	blockSize := b.blockSize
	var result []byte

	idx := 0
	for idx < len(dirents) {
		// Figure out how many entries fit in this block.
		// Each entry needs 12 bytes header + name length.
		// First pass: try to fit as many as possible.
		count := 0
		totalNameLen := 0
		for i := idx; i < len(dirents); i++ {
			headerSize := (count + 1) * ondisk.DirentSize
			nameSize := totalNameLen + len(dirents[i].name) + 1 // +1 for null terminator on last
			// Actually, only the last entry needs a null terminator but we
			// need space for all names. Intermediate names don't need null.
			// Recalculate: names are packed contiguously, last null-terminated.
			nameNeed := totalNameLen + len(dirents[i].name)
			if i == len(dirents)-1 || headerSize+nameNeed+1 >= blockSize {
				// This would be the last entry in the block (either last overall or block is full).
				nameNeed++ // null terminator
			}
			_ = nameSize
			if headerSize+nameNeed > blockSize {
				break
			}
			count++
			totalNameLen += len(dirents[i].name)
		}

		if count == 0 {
			// Name too long for a single block, force it in.
			count = 1
		}

		entries := dirents[idx : idx+count]
		nameStart := count * ondisk.DirentSize

		// Build block.
		block := make([]byte, blockSize)
		nameOff := nameStart
		for i, de := range entries {
			off := i * ondisk.DirentSize
			// Write NID (8 bytes LE).
			binary.LittleEndian.PutUint64(block[off:], de.nid)
			// Write nameoff (2 bytes LE).
			binary.LittleEndian.PutUint16(block[off+8:], uint16(nameOff))
			// Write file type (1 byte).
			block[off+10] = de.fileType
			// Reserved byte is 0.

			// Write name.
			copy(block[nameOff:], de.name)
			nameOff += len(de.name)
		}
		// Null-terminate the last name.
		if nameOff < blockSize {
			block[nameOff] = 0
			nameOff++
		}

		// For the last block of a directory, trim to actual used size
		// (used for inline tail calculation).
		if idx+count >= len(dirents) {
			// Last block: keep only the used portion.
			result = append(result, block[:nameOff]...)
		} else {
			result = append(result, block...)
		}

		idx += count
	}

	return result
}

// tryCompressInodes attempts compression on eligible file inodes.
func (b *Builder) tryCompressInodes() {
	for _, ino := range b.inodes {
		if ino.mode.IsDir() || ino.mode&fs.ModeSymlink != 0 || ino.mode&fs.ModeType != 0 {
			continue
		}
		cdata, ok := b.tryCompressFile(ino)
		if !ok {
			continue
		}
		// Switch to compressed layout.
		ino.dataLayout = ondisk.InodeCompressedFull
		ino.metaSize = computeCompressedMetaSize(len(cdata.indexEntries))
		b.compressedData[ino] = cdata
	}
}

// computeLayouts decides FLAT_INLINE vs FLAT_PLAIN for each inode.
func (b *Builder) computeLayouts() {
	for _, ino := range b.inodes {
		if ino.dataLayout == ondisk.InodeChunkBased {
			// Chunk-based: metadata = 64-byte inode + 8 bytes per chunk index.
			ino.metaSize = 64 + 8*len(ino.chunks)
			continue
		}

		dataLen := int64(len(ino.data))
		inodeHeaderSize := 64 // extended inode only
		tailSize := int(dataLen) % b.blockSize
		if dataLen == 0 {
			tailSize = 0
		}

		// If the entire data fits inline (after the 64-byte inode header,
		// still within one block from the slot-aligned inode start), use FLAT_INLINE.
		metaSize := inodeHeaderSize + tailSize

		if dataLen > 0 && tailSize > 0 && metaSize <= b.blockSize {
			ino.dataLayout = ondisk.InodeFlatInline
		} else if dataLen == 0 {
			ino.dataLayout = ondisk.InodeFlatInline
		} else {
			ino.dataLayout = ondisk.InodeFlatPlain
		}

		// Compute metaSize: inode + inline tail.
		inlineTail := 0
		if ino.dataLayout == ondisk.InodeFlatInline {
			inlineTail = tailSize
			if dataLen > 0 && dataLen < int64(b.blockSize) {
				inlineTail = int(dataLen)
			}
		}
		ino.metaSize = inodeHeaderSize + inlineTail
	}
}

// assignNIDs assigns NID values to all inodes sequentially.
// NIDs are slot indices (each slot = 32 bytes). With metaBlkAddr=0,
// the metadata area starts at byte 0 of the image. However, bytes
// 0-1023 are zero padding and bytes 1024-1167 are the superblock,
// so the first usable metadata offset is 1168 (aligned up to slot
// boundary = 1184, NID = 37).
func (b *Builder) assignNIDs() {
	b.metaBlkAddr = 0

	// Sort inodes: root first, then directories, then files/symlinks.
	sort.SliceStable(b.inodes, func(i, j int) bool {
		a, bI := b.inodes[i], b.inodes[j]
		if a.path == "/" {
			return true
		}
		if bI.path == "/" {
			return false
		}
		aDir := a.mode.IsDir()
		bDir := bI.mode.IsDir()
		if aDir != bDir {
			return aDir
		}
		return a.path < bI.path
	})

	// Start after the superblock and, for zstd, the compression-config area.
	// Superblock ends at SuperOffset + SuperBlockSize (1024 + 144 = 1168);
	// cfgs (if any) follow it.
	sbEnd := int64(ondisk.SuperOffset) + int64(ondisk.SuperBlockSize) + b.comprCfgsSize()
	slotOff := alignUp(sbEnd, ondisk.ISlotSize)

	blockSize := int64(b.blockSize)
	for _, ino := range b.inodes {
		// Align to slot boundary.
		slotOff = alignUp(slotOff, ondisk.ISlotSize)

		// EROFS forbids an inode's fixed header -- and, for FLAT_INLINE, its
		// inline tail data -- from crossing a block boundary. The 64-byte
		// header must stay within one block; an inline tail (written right
		// after the header) must too. Trailing compressed/chunk indexes may
		// cross blocks, so only the header is constrained for them. If the
		// constrained region would straddle a boundary, pad to the next block.
		noCross := int64(64)
		if ino.dataLayout == ondisk.InodeFlatInline {
			noCross = int64(ino.metaSize) // header + inline tail
		}
		if slotOff/blockSize != (slotOff+noCross-1)/blockSize {
			slotOff = alignUp(slotOff, blockSize)
		}

		ino.nid = uint64(slotOff) >> ondisk.ISlotBits
		ino.metaOff = slotOff
		slotOff += int64(ino.metaSize)
	}
}

// updateDirentNIDs updates the NID fields in directory entries now that
// NIDs have been assigned.
func (b *Builder) updateDirentNIDs() {
	// Build path->NID map.
	nidMap := make(map[string]uint64)
	for _, ino := range b.inodes {
		nidMap[ino.path] = ino.nid
	}

	for dirPath, d := range b.dirs {
		if d.inode == nil {
			continue
		}
		for i := range d.inode.children {
			de := &d.inode.children[i]
			switch de.name {
			case ".":
				de.nid = d.inode.nid
			case "..":
				parentPath := path.Dir(dirPath)
				if parentPath == "." {
					parentPath = "/"
				}
				if dirPath == "/" {
					de.nid = d.inode.nid
				} else if nid, ok := nidMap[parentPath]; ok {
					de.nid = nid
				}
			default:
				childPath := dirPath + "/" + de.name
				if dirPath == "/" {
					childPath = "/" + de.name
				}
				if nid, ok := nidMap[childPath]; ok {
					de.nid = nid
				}
			}
		}
	}
}

// layoutDataBlocks assigns block addresses for FLAT_PLAIN data.
func (b *Builder) layoutDataBlocks() {
	// Find where metadata ends.
	var metaEnd int64
	for _, ino := range b.inodes {
		end := ino.metaOff + int64(ino.metaSize)
		if end > metaEnd {
			metaEnd = end
		}
	}

	// If there is a device table, it goes between metadata and data blocks.
	if b.maxDeviceID > 0 {
		devtOff := alignUp(metaEnd, int64(ondisk.DevTSlotSize))
		devtEnd := devtOff + int64(b.maxDeviceID)*int64(ondisk.DevTSlotSize)
		if devtEnd > metaEnd {
			metaEnd = devtEnd
		}
	}

	// Data blocks start at the next block boundary after metadata (and device table).
	dataStart := alignUp(metaEnd, int64(b.blockSize))
	currentBlk := dataStart / int64(b.blockSize)

	for _, ino := range b.inodes {
		switch ino.dataLayout {
		case ondisk.InodeCompressedFull, ondisk.InodeCompressedCompact:
			// Compressed: lay out pclusters.
			if cdata, ok := b.compressedData[ino]; ok {
				b.layoutCompressedBlocks(ino, cdata, &currentBlk)
			}

		case ondisk.InodeFlatInline:
			// FLAT_INLINE: inline tail, but preceding blocks if data > blockSize.
			if int64(len(ino.data)) > int64(b.blockSize) {
				precedingSize := int64(len(ino.data)) - int64(len(ino.data))%int64(b.blockSize)
				precedingBlocks := precedingSize / int64(b.blockSize)
				ino.startBlk = uint64(currentBlk)
				currentBlk += precedingBlocks
			}

		case ondisk.InodeFlatPlain:
			dataLen := int64(len(ino.data))
			if dataLen == 0 {
				continue
			}
			numBlocks := (dataLen + int64(b.blockSize) - 1) / int64(b.blockSize)
			ino.startBlk = uint64(currentBlk)
			currentBlk += numBlocks
		}
	}

	// Record the first block past all out-of-line data so the superblock can
	// report the correct image size. This covers every layout (FLAT_PLAIN,
	// FLAT_INLINE preceding blocks, and compressed pclusters); the old maxOff
	// loop only counted FLAT_PLAIN, undercounting BlocksLo for everything else.
	b.endDataBlk = currentBlk
}

// writeMetadata writes inode metadata (extended inodes + inline tails).
func (b *Builder) writeMetadata() error {
	for _, ino := range b.inodes {
		if cdata, ok := b.compressedData[ino]; ok {
			if err := b.writeCompressedInode(ino, cdata); err != nil {
				return err
			}
			continue
		}
		if err := b.writeInode(ino); err != nil {
			return err
		}
	}
	return nil
}

// diskMtime returns the on-disk (mtime, mtime_nsec) fields for a modification time.
// When an epoch is configured the timestamp is pinned to the epoch (on-disk
// delta 0, nsec 0) so builds are reproducible; otherwise the file's own
// modification time is used, stored relative to the epoch.
func (b *Builder) diskMtime(mt time.Time) (sec int64, nsec uint32) {
	if b.epochSet {
		return 0, 0
	}
	return mt.Unix() - b.epoch, uint32(mt.Nanosecond())
}

func (b *Builder) writeInode(ino *buildInode) error {
	if ino.dataLayout == ondisk.InodeChunkBased {
		return b.writeChunkedInode(ino)
	}

	// Build the extended inode.
	mtSec, mtNsec := b.diskMtime(ino.mtime)
	ei := ondisk.InodeExtended{
		Format:    uint16(ondisk.InodeLayoutExtended) | uint16(ino.dataLayout)<<ondisk.IDataLayoutBit,
		Mode:      erofsModeFromFS(ino.mode),
		Size:      uint64(ino.size),
		U:         uint32(ino.startBlk),
		UID:       ino.uid,
		GID:       ino.gid,
		Mtime:     mtSec,
		MtimeNsec: mtNsec,
	}

	// Set nlink. In the extended inode, nlink lives in i_nlink (offset 44);
	// the field at offset 6 (NB) is the high bits of the block address
	// (startblk_hi/blocks_hi), which is 0 for our <32-bit addresses. Writing
	// nlink there corrupts the block address under the 48-bit reading that
	// fsck.erofs/the kernel apply to extended inodes.
	if ino.mode.IsDir() {
		// nlink for dirs = 2 + number of child subdirectories.
		nlink := uint32(2)
		if d, ok := b.dirs[ino.path]; ok {
			for _, child := range d.children {
				if child.mode.IsDir() {
					nlink++
				}
			}
		}
		ei.NLink = nlink
	} else {
		ei.NLink = 1
	}
	ei.NB = 0

	// Ino field: use a sequential number based on nid.
	ei.Ino = uint32(ino.nid)

	// Serialize the 64-byte inode.
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &ei); err != nil {
		return fmt.Errorf("erofs: encoding inode %s: %w", ino.path, err)
	}

	inodeBytes := buf.Bytes()

	// Write inode at its metadata offset.
	if _, err := b.w.WriteAt(inodeBytes, ino.metaOff); err != nil {
		return fmt.Errorf("erofs: writing inode %s: %w", ino.path, err)
	}

	// Write inline tail data if FLAT_INLINE.
	if ino.dataLayout == ondisk.InodeFlatInline && len(ino.data) > 0 {
		var tailData []byte
		if int64(len(ino.data)) <= int64(b.blockSize) {
			// Entire data is inline.
			tailData = ino.data
		} else {
			// Only the tail (last partial block) is inline.
			tailStart := len(ino.data) - len(ino.data)%b.blockSize
			tailData = ino.data[tailStart:]
		}
		tailOff := ino.metaOff + 64 // right after the inode header
		if _, err := b.w.WriteAt(tailData, tailOff); err != nil {
			return fmt.Errorf("erofs: writing inline data for %s: %w", ino.path, err)
		}
	}

	return nil
}

// writeDataBlocks writes out-of-line data blocks.
func (b *Builder) writeDataBlocks() error {
	for _, ino := range b.inodes {
		if len(ino.data) == 0 {
			continue
		}

		// Write compressed pclusters.
		if cdata, ok := b.compressedData[ino]; ok {
			if err := b.writeCompressedBlocks(ino, cdata); err != nil {
				return fmt.Errorf("erofs: writing compressed blocks for %s: %w", ino.path, err)
			}
			continue
		}

		switch ino.dataLayout {
		case ondisk.InodeFlatPlain:
			off := int64(ino.startBlk) * int64(b.blockSize)
			// Pad data to block boundary.
			padded := ino.data
			rem := len(padded) % b.blockSize
			if rem != 0 {
				padded = append(padded, make([]byte, b.blockSize-rem)...)
			}
			if _, err := b.w.WriteAt(padded, off); err != nil {
				return fmt.Errorf("erofs: writing data blocks for %s: %w", ino.path, err)
			}

		case ondisk.InodeFlatInline:
			if int64(len(ino.data)) > int64(b.blockSize) {
				// Write preceding full blocks.
				precedingSize := len(ino.data) - len(ino.data)%b.blockSize
				off := int64(ino.startBlk) * int64(b.blockSize)
				if _, err := b.w.WriteAt(ino.data[:precedingSize], off); err != nil {
					return fmt.Errorf("erofs: writing preceding blocks for %s: %w", ino.path, err)
				}
			}
			// Inline tail is already written in writeMetadata.
		}
	}
	return nil
}

// writeSuperblock writes the EROFS superblock at offset 1024.
func (b *Builder) writeSuperblock() error {
	// Find root NID.
	rootNID := uint64(0)
	if d, ok := b.dirs["/"]; ok && d.inode != nil {
		rootNID = d.inode.nid
	}

	// Count total inodes.
	totalInodes := uint64(len(b.inodes))

	// Compute total blocks. maxOff covers the metadata region; endDataBlk
	// (captured in layoutDataBlocks) covers every out-of-line data extent
	// regardless of layout.
	var maxOff int64
	for _, ino := range b.inodes {
		end := ino.metaOff + int64(ino.metaSize)
		if end > maxOff {
			maxOff = end
		}
	}
	totalBlocks := uint32((maxOff + int64(b.blockSize) - 1) / int64(b.blockSize))
	if uint32(b.endDataBlk) > totalBlocks {
		totalBlocks = uint32(b.endDataBlk)
	}

	// Write device table if we have extra devices.
	var devtSlotOff uint16
	var extraDevices uint16
	if b.maxDeviceID > 0 {
		extraDevices = b.maxDeviceID
		var metaEnd int64
		for _, ino := range b.inodes {
			end := ino.metaOff + int64(ino.metaSize)
			if end > metaEnd {
				metaEnd = end
			}
		}
		devtOff := alignUp(metaEnd, int64(ondisk.DevTSlotSize))
		devtSlotOff = uint16(devtOff / int64(ondisk.DevTSlotSize))

		// In flat mode, compute cumulative unified addresses starting after
		// the image's own blocks.
		var uniAddr uint64
		if b.flatDev {
			uniAddr = uint64(totalBlocks)
		}

		for i := uint16(1); i <= extraDevices; i++ {
			info := b.blobInfos[i]
			slot := ondisk.DeviceSlot{
				Tag:      info.Tag,
				BlocksLo: uint32(info.Blocks),
				BlocksHi: uint32(info.Blocks >> 32),
			}
			if b.flatDev {
				slot.UniAddrLo = uint32(uniAddr)
				slot.UniAddrHi = uint16(uniAddr >> 32)
				uniAddr += info.Blocks
			}
			var slotBuf bytes.Buffer
			if err := binary.Write(&slotBuf, binary.LittleEndian, &slot); err != nil {
				return fmt.Errorf("erofs: encoding device slot %d: %w", i, err)
			}
			off := devtOff + int64(i-1)*int64(ondisk.DevTSlotSize)
			if _, err := b.w.WriteAt(slotBuf.Bytes(), off); err != nil {
				return fmt.Errorf("erofs: writing device slot %d: %w", i, err)
			}
		}
		// Include device table in total image size.
		devtEnd := devtOff + int64(extraDevices)*int64(ondisk.DevTSlotSize)
		if devtEnd > maxOff {
			maxOff = devtEnd
		}
		// Recompute totalBlocks after device table.
		totalBlocks = uint32((maxOff + int64(b.blockSize) - 1) / int64(b.blockSize))
	}

	buildTime := uint32(0)
	if !b.epochSet {
		buildTime = uint32(time.Now().Unix() - b.epoch)
	}

	sb := ondisk.SuperBlock{
		Magic:           ondisk.SuperMagic,
		FeatureCompat:   ondisk.FeatureCompatSBChksum | ondisk.FeatureCompatMtime,
		BlkSzBits:       b.blkSzBits,
		RootNID2B:       uint16(rootNID),
		Inos:            totalInodes,
		Epoch:           b.epoch,
		BlocksLo:        totalBlocks,
		MetaBlkAddr:     b.metaBlkAddr,
		FeatureIncompat: b.computeIncompatFeatures(),
		BuildTime:       buildTime,
		AvailComprAlgs:  b.computeComprAlgs(),
		ExtraDevices:    extraDevices,
		DevtSlotOff:     devtSlotOff,
	}

	// When a compression-config area is present, readers locate it at
	// SuperOffset + 128 + sb_extslots*16. The builder writes it at
	// SuperOffset + SuperBlockSize (1024+144), so advertise one extra slot
	// (128 + 1*16 = 144) to make the offsets agree.
	if b.comprCfgsSize() > 0 {
		sb.SBExtSlots = (ondisk.SuperBlockSize - 128) / ondisk.SBExtSlotSize
	}

	// Also set RootNID8B for larger NID values.
	sb.RootNID8B = rootNID

	// Serialize superblock.
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &sb); err != nil {
		return fmt.Errorf("erofs: encoding superblock: %w", err)
	}
	sbBytes := buf.Bytes()

	// Write the superblock (without checksum first).
	if _, err := b.w.WriteAt(sbBytes, ondisk.SuperOffset); err != nil {
		return fmt.Errorf("erofs: writing superblock: %w", err)
	}

	// Compute CRC32-C checksum.
	// Read back the full first block to compute the checksum.
	block := make([]byte, b.blockSize)
	if r, ok := b.w.(io.ReaderAt); ok {
		if _, err := r.ReadAt(block, 0); err != nil {
			return fmt.Errorf("erofs: reading first block for checksum: %w", err)
		}
	} else {
		// If the writer is not also a reader, we need to reconstruct the block.
		// Write zeros + superblock into our buffer.
		copy(block[ondisk.SuperOffset:], sbBytes)
	}

	// CRC input starts after the checksum field.
	// Checksum is at offset 1024+4 (4 bytes). Data starts at 1024+8.
	start := ondisk.SuperOffset + 8
	crc := ^crc32.Update(^uint32(ondisk.SBChecksumSeed), crc32cTable, block[start:b.blockSize])

	// Write the checksum.
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	if _, err := b.w.WriteAt(crcBuf[:], ondisk.SuperOffset+4); err != nil {
		return fmt.Errorf("erofs: writing checksum: %w", err)
	}

	return nil
}

// computeIncompatFeatures returns the feature_incompat flags for the superblock.
func (b *Builder) computeIncompatFeatures() uint32 {
	var flags uint32
	if len(b.compressedData) > 0 {
		flags |= ondisk.FeatureIncompatZeroPadding
		if b.compression == CompressionZstd {
			flags |= ondisk.FeatureIncompatComprCfgs
		}
		for _, cdata := range b.compressedData {
			for _, e := range cdata.indexEntries {
				if e.Type() == ondisk.LClusterTypeNonHead {
					// Same bit value as COMPR_CFGS (0x2); harmless to OR again.
					flags |= ondisk.FeatureIncompatBigPCluster
					break
				}
			}
		}
	}
	if b.chunkBits > 0 {
		flags |= ondisk.FeatureIncompatChunkedFile
	}
	if b.maxDeviceID > 0 {
		flags |= ondisk.FeatureIncompatDeviceTable
	}
	return flags
}

// comprAlgID returns the on-disk compression algorithm id for the builder's
// selected algorithm (e.g. ondisk.CompressionLZ4, ondisk.CompressionZstd).
// Only valid for compressed inodes; it is never called when compression is
// CompressionNone.
func (b *Builder) comprAlgID() uint8 {
	return uint8(b.compression)
}

// comprCfgsSize returns the number of bytes occupied by the compression-config
// area that immediately follows the superblock. When EROFS_FEATURE_INCOMPAT_-
// COMPR_CFGS is set (which the big-pcluster bit implies, since they share a
// bit), readers expect one record per available algorithm: a __le16 size
// followed by that algorithm's config struct.
func (b *Builder) comprCfgsSize() int64 {
	if len(b.compressedData) == 0 {
		return 0
	}
	switch b.compression {
	case CompressionZstd:
		return 2 + int64(binary.Size(ondisk.ZstdCfgs{}))
	case CompressionAutoLZ4:
		return 2 + int64(binary.Size(ondisk.LZ4Cfgs{}))
	default:
		return 0
	}
}

// writeComprCfgs writes the compression-config area. Readers (kernel and
// erofs-utils) locate it at EROFS_SUPER_OFFSET + 128 + sb_extslots*16; the
// builder sets sb_extslots so that resolves to EROFS_SUPER_OFFSET +
// SuperBlockSize (= 1024+144), where this writes. Must be called before
// writeSuperblock, because the superblock checksum covers this region.
func (b *Builder) writeComprCfgs() error {
	if b.comprCfgsSize() == 0 {
		return nil
	}

	var rec any
	switch b.compression {
	case CompressionZstd:
		// A big pcluster decompresses to pclusterSize bytes, so the zstd window
		// tracks the pcluster size. On disk the window log is stored minus
		// ZSTD_WINDOWLOG_ABSOLUTEMIN (10).
		wbits := int(b.pclusterBitsEff())
		if wbits < 10 {
			wbits = 10
		}
		rec = &ondisk.ZstdCfgs{WindowLog: uint8(wbits - 10)}
	case CompressionAutoLZ4:
		// max_distance is the LZ4 window (kernel default 65535);
		// max_pclusterblks bounds the big-pcluster physical size in blocks.
		rec = &ondisk.LZ4Cfgs{
			MaxDistance:   0xFFFF,
			MaxPClusterBs: uint16(b.pclusterLclustersEff()),
		}
	default:
		return nil
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, uint16(binary.Size(rec))); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, rec); err != nil {
		return err
	}
	off := int64(ondisk.SuperOffset) + int64(ondisk.SuperBlockSize)
	if _, err := b.w.WriteAt(buf.Bytes(), off); err != nil {
		return err
	}
	return nil
}

// computeComprAlgs returns the available compression algorithms bitmap.
func (b *Builder) computeComprAlgs() uint16 {
	if len(b.compressedData) == 0 {
		return 0
	}
	return 1 << b.comprAlgID()
}

// fileTypeFromMode converts fs.FileMode to an EROFS directory entry file type.
func fileTypeFromMode(mode fs.FileMode) uint8 {
	switch {
	case mode.IsDir():
		return ondisk.FTDir
	case mode&fs.ModeSymlink != 0:
		return ondisk.FTSymlink
	case mode&fs.ModeNamedPipe != 0:
		return ondisk.FTFIFO
	case mode&fs.ModeSocket != 0:
		return ondisk.FTSock
	case mode&fs.ModeDevice != 0:
		if mode&fs.ModeCharDevice != 0 {
			return ondisk.FTChrDev
		}
		return ondisk.FTBlkDev
	default:
		return ondisk.FTRegFile
	}
}

// erofsModeFromFS converts an fs.FileMode to the EROFS on-disk mode field.
func erofsModeFromFS(mode fs.FileMode) uint16 {
	perm := uint16(mode.Perm())
	switch {
	case mode.IsDir():
		return 0o040000 | perm
	case mode&fs.ModeSymlink != 0:
		return 0o120000 | perm
	case mode&fs.ModeNamedPipe != 0:
		return 0o010000 | perm
	case mode&fs.ModeSocket != 0:
		return 0o140000 | perm
	case mode&fs.ModeDevice != 0:
		if mode&fs.ModeCharDevice != 0 {
			return 0o020000 | perm
		}
		return 0o060000 | perm
	default:
		return 0o100000 | perm
	}
}

// cleanPath normalizes a path for the image. Always uses "/" prefix.
func cleanPath(p string) string {
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// alignUp rounds v up to the next multiple of a.
func alignUp(v, a int64) int64 {
	return (v + a - 1) &^ (a - 1)
}
