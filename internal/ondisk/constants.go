// Package ondisk defines EROFS on-disk format constants and structures.
//
// All definitions are derived from the Linux kernel header:
// fs/erofs/erofs_fs.h
package ondisk

// Superblock location and magic.
const (
	SuperOffset = 1024
	SuperMagic  = 0xE0F5E1E2
	// SuperBlockSize is the on-disk size of SuperBlock in bytes.
	SuperBlockSize = 144
)

// Inode slot size.
const (
	ISlotBits = 5  // log2(32)
	ISlotSize = 32 // bytes per inode slot
)

// Compatible feature flags (feature_compat).
const (
	FeatureCompatSBChksum          = 0x00000001
	FeatureCompatMtime             = 0x00000002
	FeatureCompatXattrFilter       = 0x00000004
	FeatureCompatSharedEAInMetabox = 0x00000008
	FeatureCompatPlainXattrPfx     = 0x00000010
)

// Incompatible feature flags (feature_incompat).
const (
	FeatureIncompatZeroPadding   = 0x00000001
	FeatureIncompatComprCfgs     = 0x00000002
	FeatureIncompatBigPCluster   = 0x00000002
	FeatureIncompatChunkedFile   = 0x00000004
	FeatureIncompatDeviceTable   = 0x00000008
	FeatureIncompatComprHead2    = 0x00000008
	FeatureIncompatZTailPacking  = 0x00000010
	FeatureIncompatFragments     = 0x00000020
	FeatureIncompatDedupe        = 0x00000020
	FeatureIncompatXattrPrefixes = 0x00000040
	FeatureIncompat48Bit         = 0x00000080
	FeatureIncompatMetabox       = 0x00000100

	AllFeatureIncompat = (FeatureIncompatMetabox << 1) - 1
)

// Inode data layouts (bits 1-3 of i_format).
const (
	InodeFlatPlain         = 0
	InodeCompressedFull    = 1
	InodeFlatInline        = 2
	InodeCompressedCompact = 3
	InodeChunkBased        = 4
	InodeDataLayoutMax     = 5
)

// Inode format bit positions and masks.
const (
	IVersionBit    = 0
	IDataLayoutBit = 1
	INLink1Bit     = 4
	IDotOmittedBit = 4

	IVersionMask    = 0x01
	IDataLayoutMask = 0x07
	IAll            = 0x1F // bits 0-4
)

// Inode layout versions.
const (
	InodeLayoutCompact  = 0 // 32 bytes
	InodeLayoutExtended = 1 // 64 bytes
)

// Chunk format flags.
const (
	ChunkFormatBlkBitsMask = 0x001F
	ChunkFormatIndexes     = 0x0020
	ChunkFormat48Bit       = 0x0040
)

// Directory entry file types.
const (
	FTUnknown = 0
	FTRegFile = 1
	FTDir     = 2
	FTChrDev  = 3
	FTBlkDev  = 4
	FTFIFO    = 5
	FTSock    = 6
	FTSymlink = 7
)

// Sizes.
const (
	NameLen           = 255
	SBExtSlotSize     = 16
	DevTSlotSize      = 128
	BlockMapEntrySize = 4
	DirentSize        = 12
)

// NullAddr represents a hole/sparse chunk.
const NullAddr = 0xFFFFFFFF

// CRC32-C seed for superblock checksum.
const SBChecksumSeed = 0x5045B54A

// Compression algorithm IDs.
const (
	CompressionLZ4     = 0
	CompressionLZMA    = 1
	CompressionDeflate = 2
	CompressionZstd    = 3
	CompressionMax     = 4
)

// Compression limits.
const (
	PClusterMaxSize  = 1 << 20      // 1 MiB
	PClusterMaxDSize = 12 * 1 << 20 // 12 MiB
)

// Compression map header advise flags.
const (
	AdviseCompacted2B        = 0x0001
	AdviseExtents            = 0x0001 // same bit, different layout context
	AdviseBigPCluster1       = 0x0002
	AdviseBigPCluster2       = 0x0004
	AdviseInlinePCluster     = 0x0008
	AdviseInterlacedPCluster = 0x0010
	AdviseFragmentPCluster   = 0x0020
)

// Lcluster types (bits 0-1 of di_advise).
const (
	LClusterTypePlain   = 0
	LClusterTypeHead1   = 1
	LClusterTypeNonHead = 2
	LClusterTypeHead2   = 3
)

// Lcluster index flags.
const (
	LILClusterTypeMask = 0x03
	LID0CBlkCnt        = 1 << 11
	LIPartialRef       = 1 << 15
)

// Fragment constants.
const (
	FragmentInodeBit = 7
)

// Xattr name indices.
const (
	XattrIndexUser            = 1
	XattrIndexPosixACLAccess  = 2
	XattrIndexPosixACLDefault = 3
	XattrIndexTrusted         = 4
	XattrIndexLustre          = 5
	XattrIndexSecurity        = 6
)

// Xattr long prefix flag.
const (
	XattrLongPrefix     = 0x80
	XattrLongPrefixMask = 0x7F
)

// Xattr filter.
const (
	XattrFilterBits    = 32
	XattrFilterDefault = 0xFFFFFFFF
	XattrFilterSeed    = 0x25BBE08F
)

// Dirent NID metabox bit.
const (
	DirentNIDMetaboxBit = 63
	DirentNIDMask       = (1 << 63) - 1
)
