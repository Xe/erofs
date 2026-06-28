package ondisk

// SuperBlock is the on-disk superblock structure (144 bytes at offset 1024).
// Matches struct erofs_super_block in erofs_fs.h.
type SuperBlock struct {
	Magic           uint32
	Checksum        uint32
	FeatureCompat   uint32
	BlkSzBits       uint8
	SBExtSlots      uint8
	RootNID2B       uint16 // union: rootnid_2b / blocks_hi (48BIT mode)
	Inos            uint64
	Epoch           int64
	FixedNsec       uint32
	BlocksLo        uint32
	MetaBlkAddr     uint32
	XattrBlkAddr    uint32
	UUID            [16]byte
	VolumeName      [16]byte
	FeatureIncompat uint32
	AvailComprAlgs  uint16 // union: available_compr_algs / lz4_max_distance
	ExtraDevices    uint16
	DevtSlotOff     uint16
	DirBlkBits      uint8
	XattrPfxCount   uint8
	XattrPfxStart   uint32
	PackedNID       uint64
	XattrFilterRsvd uint8
	Reserved        [3]byte
	BuildTime       uint32
	RootNID8B       uint64
	Reserved2       uint64
	MetaboxNID      uint64
	Reserved3       uint64
}

// InodeCompact is the 32-byte compact on-disk inode (version=0).
type InodeCompact struct {
	Format      uint16
	XattrICount uint16
	Mode        uint16
	NB          uint16 // union: nlink / startblk_hi / blocks_hi
	Size        uint32
	Mtime       uint32
	U           uint32 // union: startblk_lo / blocks_lo / rdev / chunk_info
	Ino         uint32
	UID         uint16
	GID         uint16
	Reserved    uint32
}

// InodeExtended is the 64-byte extended on-disk inode (version=1).
type InodeExtended struct {
	Format      uint16
	XattrICount uint16
	Mode        uint16
	NB          uint16 // union: nlink / startblk_hi / blocks_hi
	Size        uint64
	U           uint32 // union: startblk_lo / blocks_lo / rdev / chunk_info
	Ino         uint32
	UID         uint32
	GID         uint32
	Mtime       int64
	MtimeNsec   uint32
	NLink       uint32
	Reserved2   [16]byte
}

// Dirent is a 12-byte on-disk directory entry.
type Dirent struct {
	NID      uint64
	NameOff  uint16
	FileType uint8
	Reserved uint8
}

// ChunkIndex is an 8-byte chunk index entry.
type ChunkIndex struct {
	StartBlkHi uint16
	DeviceID   uint16
	StartBlkLo uint32
}

// DeviceSlot is a 128-byte device table entry.
type DeviceSlot struct {
	Tag       [64]byte
	BlocksLo  uint32
	UniAddrLo uint32
	BlocksHi  uint32
	UniAddrHi uint16
	Reserved  [50]byte
}

// MapHeader is the 8-byte compression map header (z_erofs_map_header).
type MapHeader struct {
	Union1        uint32 // h_fragmentoff / h_idata_size / h_extents_lo
	Advise        uint16 // h_advise
	AlgorithmType uint8  // h_algorithmtype (bits 0-3: HEAD1, bits 4-7: HEAD2)
	ClusterBits   uint8  // h_clusterbits (bits 0-3: lcluster bits - blkszbits)
}

// LClusterIndex is an 8-byte lcluster index entry (z_erofs_lcluster_index).
type LClusterIndex struct {
	Advise     uint16
	ClusterOfs uint16
	Union      uint32 // blkaddr (HEAD) or delta[0]|delta[1]<<16 (NONHEAD)
}

// XattrIBodyHeader is the 12-byte inline xattr body header.
type XattrIBodyHeader struct {
	NameFilter  uint32
	SharedCount uint8
	Reserved    [7]byte
	// Followed by SharedCount uint32 entries, then inline xattr entries.
}

// XattrEntry is a variable-length xattr entry header (4 bytes fixed).
type XattrEntry struct {
	NameLen   uint8
	NameIndex uint8
	ValueSize uint16
	// Followed by NameLen bytes of name, then ValueSize bytes of value.
}

// LZ4Cfgs is the LZ4 compression configuration (14 bytes).
type LZ4Cfgs struct {
	MaxDistance   uint16
	MaxPClusterBs uint16
	Reserved      [10]byte
}

// ZstdCfgs is the Zstandard compression configuration (32 bytes), matching
// struct z_erofs_zstd_cfgs in erofs_fs.h. On disk it is preceded by a __le16
// size field in the compression-config area that immediately follows the
// superblock. WindowLog is the ZSTD window log minus ZSTD_WINDOWLOG_ABSOLUTEMIN.
type ZstdCfgs struct {
	Format    uint8
	WindowLog uint8
	Reserved  [30]byte
}

// HeadAlgorithm extracts the HEAD1 algorithm from h_algorithmtype.
func (h *MapHeader) HeadAlgorithm() uint8 {
	return h.AlgorithmType & 0x0F
}

// Head2Algorithm extracts the HEAD2 algorithm from h_algorithmtype.
func (h *MapHeader) Head2Algorithm() uint8 {
	return h.AlgorithmType >> 4
}

// LClusterBits returns the lcluster bits offset from blkszbits.
func (h *MapHeader) LClusterBits() uint8 {
	return h.ClusterBits & 0x0F
}

// IsFragmentInode returns true if the entire file is packed into the packed inode.
func (h *MapHeader) IsFragmentInode() bool {
	return h.ClusterBits&(1<<FragmentInodeBit) != 0
}

// FragmentOff returns the fragment data offset.
func (h *MapHeader) FragmentOff() uint32 {
	return h.Union1
}

// IDataSize returns the inline tail-packing data size.
func (h *MapHeader) IDataSize() uint16 {
	return uint16(h.Union1 >> 16)
}

// BlkAddr returns the block address from a HEAD lcluster entry.
func (e *LClusterIndex) BlkAddr() uint32 {
	return e.Union
}

// Delta0 returns delta[0] from a NONHEAD lcluster entry.
func (e *LClusterIndex) Delta0() uint16 {
	return uint16(e.Union & 0xFFFF)
}

// Delta1 returns delta[1] from a NONHEAD lcluster entry.
func (e *LClusterIndex) Delta1() uint16 {
	return uint16(e.Union >> 16)
}

// Type returns the lcluster type (bits 0-1 of advise).
func (e *LClusterIndex) Type() uint8 {
	return uint8(e.Advise & LILClusterTypeMask)
}
