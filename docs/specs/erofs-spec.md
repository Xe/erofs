# EROFS Go Implementation Specification

## Context

This specification describes how to implement EROFS (Enhanced Read-Only File System) in Go as both an `fs.FS` reader and an image builder. EROFS is a Linux kernel filesystem designed for read-only use cases (container images, system partitions, live media). The goal is byte-compatible interoperability with the Linux kernel's EROFS driver and mkfs.erofs.

All on-disk structures are derived from `/Users/cadey/Code/Xe/erofs/kernel/fs/erofs/erofs_fs.h`. All behavioral details are derived from the kernel driver source in `/Users/cadey/Code/Xe/erofs/kernel/fs/erofs/`.

---

## 1. Conventions

- All multi-byte integers are **little-endian**.
- Byte offsets are zero-indexed from image start unless stated otherwise.
- "Block" = filesystem block of `1 << blkszbits` bytes.
- "NID" = node identifier, an index into 32-byte inode slots in the metadata area.
- Go types use `encoding/binary` with `binary.LittleEndian`.
- CRC32-C uses the Castagnoli polynomial: `crc32.MakeTable(crc32.Castagnoli)`.
- The image is accessed through `io.ReaderAt` (reader) or `io.WriterAt` (builder).

---

## 2. On-Disk Constants

```
EROFS_SUPER_OFFSET           = 1024        // superblock location in image
EROFS_SUPER_MAGIC_V1         = 0xE0F5E1E2  // magic number (little-endian)
EROFS_SB_EXTSLOT_SIZE        = 16          // bytes per extension slot
EROFS_ISLOT_BITS             = 5           // inode slot = 32 bytes = 1 << 5
EROFS_NAME_LEN               = 255         // max filename length
EROFS_NULL_ADDR              = 0xFFFFFFFF  // represents a hole/sparse chunk
EROFS_DEVT_SLOT_SIZE         = 128         // device table slot size
EROFS_BLOCK_MAP_ENTRY_SIZE   = 4           // simple block address entry

Z_EROFS_PCLUSTER_MAX_SIZE    = 1048576     // 1 MiB max compressed pcluster
Z_EROFS_PCLUSTER_MAX_DSIZE   = 12582912   // 12 MiB max decompressed pcluster
Z_EROFS_LZMA_MAX_DICT_SIZE   = 8388608    // 8 MiB
Z_EROFS_ZSTD_MAX_DICT_SIZE   = 1048576    // 1 MiB
```

### 2.1 Feature Flags

**Compatible (`feature_compat`)** -- readers MAY ignore unknown bits:

| Mask         | Name                   | Description                        |
| ------------ | ---------------------- | ---------------------------------- |
| `0x00000001` | `SB_CHKSUM`            | CRC32-C checksum present           |
| `0x00000002` | `MTIME`                | Per-inode mtime in extended inodes |
| `0x00000004` | `XATTR_FILTER`         | Bloom filter for xattr lookup      |
| `0x00000008` | `SHARED_EA_IN_METABOX` | Shared xattrs in metabox           |
| `0x00000010` | `PLAIN_XATTR_PFX`      | Plain xattr prefixes               |

**Incompatible (`feature_incompat`)** -- readers MUST reject unknown bits:

| Mask         | Name                           | Description                          |
| ------------ | ------------------------------ | ------------------------------------ |
| `0x00000001` | `ZERO_PADDING`                 | Compressed blocks zero-padded at end |
| `0x00000002` | `COMPR_CFGS` / `BIG_PCLUSTER`  | Compression configs / big pclusters  |
| `0x00000004` | `CHUNKED_FILE`                 | Chunk-based inode layout             |
| `0x00000008` | `DEVICE_TABLE` / `COMPR_HEAD2` | Device table / HEAD2 compression     |
| `0x00000010` | `ZTAILPACKING`                 | Compressed tail-packing inline       |
| `0x00000020` | `FRAGMENTS` / `DEDUPE`         | Fragment packing / deduplication     |
| `0x00000040` | `XATTR_PREFIXES`               | Long xattr name prefixes             |
| `0x00000080` | `48BIT`                        | 48-bit block addressing              |
| `0x00000100` | `METABOX`                      | Metadata compression                 |

`EROFS_ALL_FEATURE_INCOMPAT = (0x0100 << 1) - 1 = 0x01FF`

---

## 3. Superblock (144 bytes at offset 1024)

### 3.1 On-Disk Layout

Source: `erofs_fs.h:53-91`

All offsets relative to superblock start (absolute offset 1024):

| Offset | Size | Field                      | Type     | Description                                   |
| ------ | ---- | -------------------------- | -------- | --------------------------------------------- |
| 0x00   | 4    | `magic`                    | `u32le`  | Must be `0xE0F5E1E2`                          |
| 0x04   | 4    | `checksum`                 | `u32le`  | CRC32-C; see section 3.2                      |
| 0x08   | 4    | `feature_compat`           | `u32le`  | Compatible feature flags                      |
| 0x0C   | 1    | `blkszbits`                | `u8`     | Block size = `1 << blkszbits`; minimum 9      |
| 0x0D   | 1    | `sb_extslots`              | `u8`     | SB size = `128 + sb_extslots * 16`            |
| 0x0E   | 2    | `rootnid_2b` / `blocks_hi` | `u16le`  | Root NID (non-48bit) or blocks count MSB      |
| 0x10   | 8    | `inos`                     | `u64le`  | Total valid inode count                       |
| 0x18   | 8    | `epoch`                    | `i64le`  | Base seconds for compact inode timestamps     |
| 0x20   | 4    | `fixed_nsec`               | `u32le`  | Fixed nanoseconds for compact inodes          |
| 0x24   | 4    | `blocks_lo`                | `u32le`  | Total blocks count LSB                        |
| 0x28   | 4    | `meta_blkaddr`             | `u32le`  | Start block of metadata area                  |
| 0x2C   | 4    | `xattr_blkaddr`            | `u32le`  | Start block of shared xattr area              |
| 0x30   | 16   | `uuid`                     | `[16]u8` | Volume UUID                                   |
| 0x40   | 16   | `volume_name`              | `[16]u8` | Volume label (null-terminated)                |
| 0x50   | 4    | `feature_incompat`         | `u32le`  | Incompatible feature flags                    |
| 0x54   | 2    | `available_compr_algs`     | `u16le`  | Union: compression bitmap or LZ4 max distance |
| 0x56   | 2    | `extra_devices`            | `u16le`  | Number of extra devices                       |
| 0x58   | 2    | `devt_slotoff`             | `u16le`  | Device table slot offset                      |
| 0x5A   | 1    | `dirblkbits`               | `u8`     | Must be 0                                     |
| 0x5B   | 1    | `xattr_prefix_count`       | `u8`     | Long xattr prefix count                       |
| 0x5C   | 4    | `xattr_prefix_start`       | `u32le`  | Start of long xattr prefixes                  |
| 0x60   | 8    | `packed_nid`               | `u64le`  | NID of packed/fragment inode                  |
| 0x68   | 1    | `xattr_filter_reserved`    | `u8`     | Reserved                                      |
| 0x69   | 3    | `reserved`                 | `[3]u8`  | Must be 0                                     |
| 0x6C   | 4    | `build_time`               | `u32le`  | Seconds added to epoch for mkfs time          |
| 0x70   | 8    | `rootnid_8b`               | `u64le`  | (48BIT mode) Root directory NID               |
| 0x78   | 8    | `reserved2`                | `u64le`  | Reserved                                      |
| 0x80   | 8    | `metabox_nid`              | `u64le`  | (METABOX mode) NID of metabox inode           |
| 0x88   | 8    | `reserved3`                | `u64le`  | Alignment padding                             |

Go struct for `encoding/binary.Read`:

```go
type SuperBlock struct {
    Magic            uint32
    Checksum         uint32
    FeatureCompat    uint32
    BlkSzBits        uint8
    SBExtSlots       uint8
    RootNID2B        uint16   // union with BlocksHi in 48-bit mode
    Inos             uint64
    Epoch            int64
    FixedNsec        uint32
    BlocksLo         uint32
    MetaBlkAddr      uint32
    XattrBlkAddr     uint32
    UUID             [16]byte
    VolumeName       [16]byte
    FeatureIncompat  uint32
    AvailComprAlgs   uint16   // union with LZ4MaxDistance
    ExtraDevices     uint16
    DevtSlotOff      uint16
    DirBlkBits       uint8
    XattrPrefixCount uint8
    XattrPrefixStart uint32
    PackedNID        uint64
    XattrFilterRsvd  uint8
    Reserved         [3]byte
    BuildTime        uint32
    RootNID8B        uint64
    Reserved2        uint64
    MetaboxNID       uint64
    Reserved3        uint64
}
```

### 3.2 CRC32-C Checksum

Source: `super.c:40-57`

Polynomial: CRC32-C (Castagnoli, `0x1EDC6F41`).
Seed: `0x5045B54A`.

**Verification procedure:**

1. Read the entire first block (bytes `[0, blocksize)`).
2. Compute: `len = blocksize - EROFS_SUPER_OFFSET - 8` (skip past magic + checksum fields).
3. CRC input starts at absolute offset `1024 + 8` (byte after the checksum field), length `len`.
4. `crc = crc32c(seed=0x5045B54A, data[1024+8 : blocksize])`.
5. Compare with `le32(checksum)`.

**Creation procedure:**

1. Write superblock with `checksum = 0`.
2. Compute CRC as above.
3. Store result in the `checksum` field (little-endian).

```go
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

func computeSBChecksum(block []byte, blkszbits uint8) uint32 {
    blocksize := 1 << blkszbits
    start := 1024 + 8 // past magic(4) + checksum(4)
    return crc32.Update(0x5045B54A, crc32cTable, block[start:blocksize])
}
```

### 3.3 Compression Configuration Storage

Source: `super.c:133-137` and `super.c:291-355`

When `COMPR_CFGS` is set in `feature_incompat`, compression configs are stored after the superblock. Offset: `EROFS_SUPER_OFFSET + 128 + sb_extslots * 16`.

Each config is read using the metadata pattern:

1. Align offset to 4 bytes.
2. Read `u16le` length field.
3. Read `length` bytes of config data.

Configs are stored for each set bit in `available_compr_algs`, lowest bit first:

| Algorithm | ID  | Config struct               | Fields                                                           |
| --------- | --- | --------------------------- | ---------------------------------------------------------------- |
| LZ4       | 0   | `z_erofs_lz4_cfgs` (14B)    | `max_distance(u16le)`, `max_pclusterblks(u16le)`, `reserved[10]` |
| LZMA      | 1   | `z_erofs_lzma_cfgs` (14B)   | `dict_size(u32le)`, `format(u16le)`, `reserved[8]`               |
| DEFLATE   | 2   | `z_erofs_deflate_cfgs` (6B) | `windowbits(u8)`, `reserved[5]`                                  |
| ZSTD      | 3   | `z_erofs_zstd_cfgs` (6B)`   | `format(u8)`, `windowlog(u8)`, `reserved[4]`                     |

When `COMPR_CFGS` is NOT set, LZ4 is the only algorithm. `lz4_max_distance` comes from the superblock union field. `max_pclusterblks` defaults to 1.

---

## 4. Inodes

Source: `erofs_fs.h:103-191`, `inode.c:28-202`

### 4.1 Locating an Inode by NID

```
inode_byte_offset = meta_blkaddr * block_size + (nid << 5)
```

Where `nid << 5` is `nid * 32` (the inode slot size). An extended (64-byte) inode may span a block boundary -- implementations must handle reading across boundaries.

### 4.2 `i_format` Bit Layout

```
Bit 0:      version (0 = compact 32B, 1 = extended 64B)
Bits 1-3:   datalayout (0-4 defined, 5-7 reserved)
Bit 4:      NLINK_1 (non-dir compact: nlink=1, i_nb=startblk_hi)
            or DOT_OMITTED (directories: '.' entry omitted from disk)
Bits 5-15:  reserved, must be 0
```

Extract: `version = i_format & 0x01`, `datalayout = (i_format >> 1) & 0x07`.

`EROFS_I_ALL = 0x1F` (bits 0-4). Reject if `i_format & ~EROFS_I_ALL != 0`.

### 4.3 Compact Inode (32 bytes, version=0)

Source: `erofs_fs.h:160-173`

| Offset | Size | Field            | Type    | Notes                                       |
| ------ | ---- | ---------------- | ------- | ------------------------------------------- |
| 0x00   | 2    | `i_format`       | `u16le` | Format hints                                |
| 0x02   | 2    | `i_xattr_icount` | `u16le` | Xattr count                                 |
| 0x04   | 2    | `i_mode`         | `u16le` | POSIX file mode                             |
| 0x06   | 2    | `i_nb`           | `u16le` | nlink OR startblk_hi OR blocks_hi           |
| 0x08   | 4    | `i_size`         | `u32le` | File size (32-bit, max 4 GiB)               |
| 0x0C   | 4    | `i_mtime`        | `u32le` | Reserved / 48-bit specific                  |
| 0x10   | 4    | `i_u`            | `u32le` | startblk_lo / blocks_lo / rdev / chunk_info |
| 0x14   | 4    | `i_ino`          | `u32le` | Serial number (stat compat)                 |
| 0x18   | 2    | `i_uid`          | `u16le` | Owner UID (16-bit)                          |
| 0x1A   | 2    | `i_gid`          | `u16le` | Owner GID (16-bit)                          |
| 0x1C   | 4    | `i_reserved`     | `u32le` | Reserved                                    |

Timestamp: `seconds = epoch + i_mtime`, `nsec = fixed_nsec` (both from superblock).

### 4.4 Extended Inode (64 bytes, version=1)

Source: `erofs_fs.h:176-191`

| Offset | Size | Field            | Type     | Notes                                       |
| ------ | ---- | ---------------- | -------- | ------------------------------------------- |
| 0x00   | 2    | `i_format`       | `u16le`  | Format hints                                |
| 0x02   | 2    | `i_xattr_icount` | `u16le`  | Xattr count                                 |
| 0x04   | 2    | `i_mode`         | `u16le`  | POSIX file mode                             |
| 0x06   | 2    | `i_nb`           | `u16le`  | nlink OR startblk_hi OR blocks_hi           |
| 0x08   | 8    | `i_size`         | `u64le`  | File size (64-bit)                          |
| 0x10   | 4    | `i_u`            | `u32le`  | startblk_lo / blocks_lo / rdev / chunk_info |
| 0x14   | 4    | `i_ino`          | `u32le`  | Serial number                               |
| 0x18   | 4    | `i_uid`          | `u32le`  | Owner UID (32-bit)                          |
| 0x1C   | 4    | `i_gid`          | `u32le`  | Owner GID (32-bit)                          |
| 0x20   | 8    | `i_mtime`        | `i64le`  | Modification time (seconds)                 |
| 0x28   | 4    | `i_mtime_nsec`   | `u32le`  | Nanoseconds                                 |
| 0x2C   | 4    | `i_nlink`        | `u32le`  | Hard link count (32-bit)                    |
| 0x30   | 16   | `i_reserved2`    | `[16]u8` | Reserved                                    |

Timestamp: `seconds = i_mtime`, `nsec = i_mtime_nsec`.

### 4.5 `i_u` Union Interpretation

| Datalayout             | Interpretation | Description                       |
| ---------------------- | -------------- | --------------------------------- |
| FLAT_PLAIN (0)         | `startblk_lo`  | Starting block of contiguous data |
| COMPRESSED_FULL (1)    | `blocks_lo`    | Total compressed blocks count     |
| FLAT_INLINE (2)        | `startblk_lo`  | Starting block (preceding blocks) |
| COMPRESSED_COMPACT (3) | `blocks_lo`    | Total compressed blocks count     |
| CHUNK_BASED (4)        | `chunk_info`   | `format(u16le) + reserved(u16le)` |
| S_IFCHR / S_IFBLK      | `rdev`         | Device number                     |

For flat inodes, full starting block address:

- If `NLINK_1` bit set (compact, non-directory): `startblk = startblk_lo | (uint64(i_nb) << 32)`
- Otherwise: `startblk = startblk_lo` (i_nb is nlink)

### 4.6 Xattr Inline Size

Source: `erofs_fs.h:245-253`

```
if i_xattr_icount == 0:
    xattr_isize = 0
else:
    xattr_isize = 12 + 4 * (i_xattr_icount - 1)
```

### 4.7 Data After Inode

After each inode on disk, in order:

1. **Xattr inline body** (`xattr_isize` bytes, if nonzero)
2. **Compression map header** (8 bytes, ALIGN to 8, for compressed layouts only)
3. **Compression index / extent data** (for compressed layouts)
4. **Inline data** (for FLAT_INLINE layout: `i_size % block_size` bytes of tail data)

---

## 5. Inode Data Layouts

Source: `erofs_fs.h:93-110`, `data.c:86-173`

### 5.1 FLAT_PLAIN (layout 0)

File data occupies `ceil(i_size / block_size)` contiguous blocks starting at `startblk`.

Mapping for logical offset `la`:

```
physical_offset = startblk * block_size + la
length = i_size - la
```

If `startblk == EROFS_NULL_ADDR`, the file is a hole (all zeros).

### 5.2 FLAT_INLINE (layout 2)

All data before the last partial block is in contiguous blocks at `startblk`. The tail portion (`i_size % block_size` bytes) is stored inline after the inode metadata.

```
tail_size = i_size % block_size
preceding_size = i_size - tail_size     // = (i_size / block_size) * block_size

if la < preceding_size:
    // In preceding blocks
    physical_offset = startblk * block_size + la
    length = preceding_size - la

else:
    // In inline tail
    iloc = meta_blkaddr * block_size + (nid << 5)
    physical_offset = iloc + inode_isize + xattr_isize + (la % block_size)
    length = i_size - la
    // Flag: EROFS_MAP_META (data is in metadata area, not data area)
```

**Constraint**: Inline tail must not cross a block boundary:

```
blkoff(iloc + inode_isize + xattr_isize) + tail_size <= block_size
```

where `blkoff(x) = x & (block_size - 1)`.

If `i_size <= block_size` and the data fits inline, `startblk` is ignored and all data is inline.

### 5.3 CHUNK_BASED (layout 4)

Source: `erofs_fs.h:128-274`, `data.c:86-173`

Chunk size: `block_size << (chunk_format & EROFS_CHUNK_FORMAT_BLKBITS_MASK)` where mask = `0x001F`.

Other chunk format flags:

- `EROFS_CHUNK_FORMAT_INDEXES` (`0x0020`): Use 8-byte chunk index entries (else 4-byte block array)
- `EROFS_CHUNK_FORMAT_48BIT` (`0x0040`): 48-bit device addressing

Chunk entries are stored after inode + xattrs, aligned to entry size:

```
entry_size = 8   if (chunk_format & EROFS_CHUNK_FORMAT_INDEXES)
           = 4   otherwise

iloc = meta_blkaddr * block_size + (nid << 5)
chunk_entries_start = ALIGN(iloc + inode_isize + xattr_isize, entry_size)
chunk_number = la >> chunk_bits
entry_offset = chunk_entries_start + entry_size * chunk_number
```

**8-byte chunk index** (`erofs_inode_chunk_index`):

| Offset | Size | Field         | Type    |
| ------ | ---- | ------------- | ------- |
| 0x00   | 2    | `startblk_hi` | `u16le` |
| 0x02   | 2    | `device_id`   | `u16le` |
| 0x04   | 4    | `startblk_lo` | `u32le` |

Block address: `startblk_lo | (uint64(startblk_hi) << 32)`. If it equals `EROFS_NULL_ADDR` (masked), it is a hole.

**4-byte block map**: Each entry is a `u32le` block address. `0xFFFFFFFF` = hole.

### 5.4 Compressed Layouts (1 and 3)

See Section 8.

---

## 6. Directories

Source: `erofs_fs.h:279-293`, `dir.c:9-120`, `namei.c`

### 6.1 Directory Block Structure

Directories use FLAT_PLAIN or FLAT_INLINE layout. Each directory block contains:

1. An array of `erofs_dirent` entries (12 bytes each) at the start
2. Filename strings after the dirent array

**Dirent structure** (12 bytes):

| Offset | Size | Field       | Type    |
| ------ | ---- | ----------- | ------- |
| 0x00   | 8    | `nid`       | `u64le` |
| 0x08   | 2    | `nameoff`   | `u16le` |
| 0x0A   | 1    | `file_type` | `u8`    |
| 0x0B   | 1    | `reserved`  | `u8`    |

Entry count per block:

```
entry_count = (de[0].nameoff & (block_size - 1)) / sizeof(erofs_dirent)
            = (de[0].nameoff & (block_size - 1)) / 12
```

Filename length for entry `i`:

- If `i < entry_count - 1`: `nameoff[i+1] - nameoff[i]`
- If last entry: `strnlen(name, maxsize - nameoff[i])` where `maxsize = min(dir_isize - block_start, block_size)`

File type values:

| Value | Type             |
| ----- | ---------------- |
| 0     | Unknown          |
| 1     | Regular file     |
| 2     | Directory        |
| 3     | Character device |
| 4     | Block device     |
| 5     | FIFO             |
| 6     | Socket           |
| 7     | Symlink          |

Entries are sorted in byte-value lexicographic order within each block. Filenames are NOT null-terminated except the last in each block. Max filename: 255 bytes.

### 6.2 Directory Lookup (Binary Search)

Source: `namei.c:89-191`

Two-level binary search:

**Level 1 -- Block search** (`erofs_find_target_block`):

```
head = 0, back = num_blocks - 1
startprfx = endprfx = 0

while head <= back:
    mid = head + (back - head) / 2
    read block[mid], get first entry's name
    diff = dirnamecmp(target, first_name, &matched)
    if diff < 0:
        back = mid - 1
        endprfx = matched
    elif diff == 0:
        return block[mid]  // exact match
    else:
        head = mid + 1
        startprfx = matched
        candidate = block[mid]
        candidate_ndirents = ndirents
return candidate
```

**Level 2 -- Entry search** (`find_target_dirent`):
Binary search within the candidate block's dirent array, same prefix-optimized comparison.

**String comparison** (`erofs_dirnamecmp`): Byte-by-byte comparison tracking matched prefix length. Returns `<0`, `0`, or `>0`.

### 6.3 Directory Iteration

For `ReadDir`, iterate blocks sequentially:

1. Read `nameoff` of `de[0]` to determine `entry_count`
2. Iterate entries 0 through `entry_count - 1`
3. Extract each filename and emit `(name, file_type, nid)`

If `DOT_OMITTED` is set in `i_format` bit 4 (for directories), the `.` entry is synthesized rather than stored on disk.

---

## 7. Symlinks

Source: `inode.c:11-26`

Symlinks use FLAT_INLINE (fast symlinks) or FLAT_PLAIN layout.

**Fast symlinks** (FLAT_INLINE): The link target is stored inline after inode metadata. Read `i_size` bytes at:

```
offset = iloc + inode_isize + xattr_isize
```

This works only if the data fits within the same block:

```
blkoff(iloc) + inode_isize + xattr_isize + i_size <= block_size
```

If it does not fit inline, the symlink target is in data blocks (same as a regular file).

---

## 8. Compressed File Support

Source: `erofs_fs.h:301-437`, `zmap.c`, `zdata.c`, `decompressor.c`

### 8.1 Compression Map Header

For compressed inodes (layouts 1 and 3), the map header is at:

```
pos = ALIGN(iloc + inode_isize + xattr_isize, 8)
```

`z_erofs_map_header` (8 bytes):

| Offset | Size | Field             | Type    | Description                                                   |
| ------ | ---- | ----------------- | ------- | ------------------------------------------------------------- |
| 0x00   | 4    | `h_fragmentoff`   | `u32le` | Union: fragment offset / idata_size / extents_lo              |
| 0x04   | 2    | `h_advise`        | `u16le` | Compression advice flags                                      |
| 0x06   | 1    | `h_algorithmtype` | `u8`    | Bits 0-3: HEAD1 alg, bits 4-7: HEAD2 alg                      |
| 0x07   | 1    | `h_clusterbits`   | `u8`    | Bits 0-3: lcluster bits - blkszbits; bit 7: whole file packed |

`h_fragmentoff` union interpretation:

- As `h_fragmentoff`: fragment data offset in packed inode (when FRAGMENT_PCLUSTER)
- As `h_idata_size` (bytes 2-3): encoded size of tail-packing data (when INLINE_PCLUSTER)
- As `h_extents_lo`: extent count LSB (when EXTENTS mode)

Lcluster size: `block_size << (h_clusterbits & 0x0F)`.

Fragment inode: if bit 7 of `h_clusterbits` is set, the entire file is packed into the packed inode.

Advise flags:

| Mask     | Name                                        | Description                        |
| -------- | ------------------------------------------- | ---------------------------------- |
| `0x0001` | `COMPACTED_2B` (compact) / `EXTENTS` (full) | 2B compact index / extent metadata |
| `0x0002` | `BIG_PCLUSTER_1`                            | HEAD1 big pclusters                |
| `0x0004` | `BIG_PCLUSTER_2`                            | HEAD2/PLAIN big pclusters          |
| `0x0008` | `INLINE_PCLUSTER`                           | Tail-packed compressed data inline |
| `0x0010` | `INTERLACED_PCLUSTER`                       | PLAIN type uses interlaced layout  |
| `0x0020` | `FRAGMENT_PCLUSTER`                         | Tail extent is a fragment          |

### 8.2 Compression Algorithms

Source: `erofs_fs.h:302-308`

| ID  | Algorithm | Type   | Go library                           |
| --- | --------- | ------ | ------------------------------------ |
| 0   | LZ4       | Block  | `github.com/pierrec/lz4/v4`          |
| 1   | LZMA      | Stream | `github.com/ulikunitz/xz/lzma`       |
| 2   | DEFLATE   | Stream | `compress/flate` (stdlib)            |
| 3   | ZSTD      | Stream | `github.com/klauspost/compress/zstd` |

Algorithm configs (sizes exclude the 2-byte length prefix):

- **LZ4** (14 bytes): `max_distance(u16le)` (default 64KB), `max_pclusterblks(u16le)`, `reserved[10]`
- **LZMA** (14 bytes): `dict_size(u32le)` (4KB-8MB), `format(u16le)`, `reserved[8]`
- **DEFLATE** (6 bytes): `windowbits(u8)` (8-15), `reserved[5]`
- **ZSTD** (6 bytes): `format(u8)`, `windowlog(u8)` (actual = value + 10), `reserved[4]`

### 8.3 Lcluster Types

Source: `erofs_fs.h:386-392`

| Value | Type    | Description                                         |
| ----- | ------- | --------------------------------------------------- |
| 0     | PLAIN   | Uncompressed data stored in compressed index layout |
| 1     | HEAD1   | Head of compressed extent, algorithm from bits 0-3  |
| 2     | NONHEAD | Continuation; stores delta to HEAD                  |
| 3     | HEAD2   | Head of compressed extent, algorithm from bits 4-7  |

### 8.4 FULL Index Format (layout 1)

Source: `erofs_fs.h:417-419`, `zmap.c`

Index entries start at:

```
end = iloc + inode_isize + xattr_isize
Z_EROFS_FULL_INDEX_START = ALIGN(end, 8) + 8 + 8
```

(8 bytes for map header at `ALIGN(end, 8)`, then 8 bytes padding.)

Each `z_erofs_lcluster_index` entry (8 bytes):

| Offset | Size | Field                                    | Type    |
| ------ | ---- | ---------------------------------------- | ------- |
| 0x00   | 2    | `di_advise`                              | `u16le` |
| 0x02   | 2    | `di_clusterofs`                          | `u16le` |
| 0x04   | 4    | `blkaddr` (HEAD) or `delta[2]` (NONHEAD) | varies  |

For HEAD lclusters:

- `di_advise & 0x03`: lcluster type (HEAD1 or HEAD2 or PLAIN)
- `di_clusterofs`: byte offset within the decompressed lcluster
- `blkaddr` (bytes 4-7, `u32le`): physical block address of compressed pcluster

For NONHEAD lclusters:

- `delta[0]` (bytes 4-5, `u16le`): distance back to HEAD lcluster
- `delta[1]` (bytes 6-7, `u16le`): distance forward to next HEAD lcluster
- If `di_advise & Z_EROFS_LI_D0_CBLKCNT` (bit 11 = `0x0800`): first NONHEAD stores compressed block count as `delta[0] & ~0x0800`

Other flags:

- `Z_EROFS_LI_PARTIAL_REF` (bit 15 = `0x8000`): partial decompressed data (FULL format only)

### 8.5 COMPACT Index Format (layout 3)

Source: `zmap.c`

Index starts at:

```
ebase = ALIGN(iloc + inode_isize + xattr_isize, 8) + 8  // after map header
```

The compact format uses bit-packed entries in groups. It has three regions:

```
compacted_4b_initial = ((32 - ebase % 32) / 4) & 7
compacted_2b = 0
if COMPACTED_2B and compacted_4b_initial < totalidx:
    compacted_2b = rounddown(totalidx - compacted_4b_initial, 16)
remaining_4b = totalidx - compacted_4b_initial - compacted_2b
```

- First `compacted_4b_initial` lclusters: 4-byte amortized entries (packs of 2)
- Next `compacted_2b` lclusters: 2-byte amortized entries (packs of 16)
- Remaining: 4-byte amortized entries (packs of 2)

Each pack's last 4 bytes contain the base block address (`u32le`). Within each pack, entries are bit-encoded with:

```
lobits = max(lclusterbits, ilog2(Z_EROFS_LI_D0_CBLKCNT) + 1)  // at least 12
encodebits = ((vcnt * amortized_size) - 4) * 8 / vcnt
value = read_le32_unaligned(in + bitpos/8) >> (bitpos & 7)
lo = value & ((1 << lobits) - 1)
type = (value >> lobits) & 3
```

### 8.6 NONHEAD Delta Chain Following

To read compressed data at logical offset `la`:

1. Compute `lcn = la >> lclusterbits`
2. Load the lcluster entry at index `lcn`
3. If HEAD type: physical block address is directly available
4. If NONHEAD: follow the chain:
   ```
   while type == NONHEAD:
       lcn = lcn - delta[0]
       load lcluster entry at lcn
   ```
5. HEAD found: `pblk = blkaddr`, `clusterofs = di_clusterofs`
6. Logical start of extent: `(lcn << lclusterbits) + clusterofs`

### 8.7 Determining Pcluster Size

**Without big pcluster**: Each pcluster is exactly 1 block.

**With big pcluster**: Look at the first NONHEAD after the HEAD:

- If that NONHEAD has `D0_CBLKCNT` set: `compressed_blocks = delta[0] & ~0x0800`
- If no NONHEAD follows (next is another HEAD): pcluster is 1 block

`pcluster_physical_size = compressed_blocks * block_size`

### 8.8 Decompression Procedure

For each pcluster:

1. Determine algorithm from HEAD type:
   - PLAIN (type 0): no compression
   - HEAD1 (type 1): `h_algorithmtype & 0x0F`
   - HEAD2 (type 3): `h_algorithmtype >> 4`
2. Read `pcluster_physical_size` bytes from `pblk * block_size`
3. Decompress:
   - **LZ4**: Block decompression. If `ZERO_PADDING` feature set, trim trailing zeros first. Use `LZ4_decompress_safe` for full or `LZ4_decompress_safe_partial` for partial.
   - **DEFLATE/LZMA/ZSTD**: Stream decompression with the configured window/dict sizes.
   - **PLAIN (non-interlaced / SHIFTED)**: Data starts at `pageofs_out` offset within the pcluster. Simply copy with shift.
   - **PLAIN (INTERLACED)**: Tail portion (`block_size - pageofs_out` mod `block_size` bytes) stored first, then head portion. Rearrange during copy.
4. Extract requested bytes from decompressed output at `offset_within_extent`.

### 8.9 Tail-Packing and Fragments

**Tail-packing** (`ZTAILPACKING` + `INLINE_PCLUSTER`): Last compressed extent is stored inline after inode metadata. Size from `h_idata_size` field. Must not cross block boundary.

**Fragments** (`FRAGMENTS` + `FRAGMENT_PCLUSTER`): Data stored in packed inode (`packed_nid` from superblock). Fragment offset from `h_fragmentoff`. Entire file can be a fragment if bit 7 of `h_clusterbits` is set.

### 8.10 Random Access

Each pcluster decompresses independently -- no cross-pcluster dependencies. To read `[offset, offset+length)`:

1. Compute lcluster index from offset
2. Follow delta chain to HEAD
3. Read and decompress the pcluster
4. Extract the requested byte range from decompressed output

Multiple pclusters may need decompressing if the read spans an extent boundary.

---

## 9. Extended Attributes (Xattrs)

Source: `erofs_fs.h:193-261`, `xattr.c`

### 9.1 Inline Xattrs

After the inode structure, if `i_xattr_icount > 0`:

**Xattr ibody header** (12 bytes):

| Offset | Size | Field             | Type       |
| ------ | ---- | ----------------- | ---------- |
| 0x00   | 4    | `h_name_filter`   | `u32le`    |
| 0x04   | 1    | `h_shared_count`  | `u8`       |
| 0x05   | 7    | `h_reserved`      | `[7]u8`    |
| 0x0C   | 4\*n | `h_shared_xattrs` | `[n]u32le` |

Where `n = h_shared_count`. Inline xattr entries follow.

### 9.2 Xattr Entry (4 + variable bytes)

| Offset | Size | Field          | Type    |
| ------ | ---- | -------------- | ------- |
| 0x00   | 1    | `e_name_len`   | `u8`    |
| 0x01   | 1    | `e_name_index` | `u8`    |
| 0x02   | 2    | `e_value_size` | `u16le` |
| 0x04   | var  | `e_name`       | bytes   |
| ...    | var  | `e_value`      | bytes   |

Entries are 4-byte aligned: `entry_size = ALIGN(4 + e_name_len + e_value_size, 4)`.

Name index prefixes:

| Index | Prefix                     |
| ----- | -------------------------- |
| 1     | `user.`                    |
| 2     | `system.posix_acl_access`  |
| 3     | `system.posix_acl_default` |
| 4     | `trusted.`                 |
| 5     | `lustre.`                  |
| 6     | `security.`                |

Long prefix: if `e_name_index & 0x80`, lower 7 bits index into `xattr_prefixes[]`. Full name = `standard_prefix + infix + e_name`.

### 9.3 Shared Xattrs

Located at `xattr_blkaddr * block_size + 4 * xattr_id`. Each is an `erofs_xattr_entry`.

### 9.4 Name Filter (Bloom Filter)

`h_name_filter` is a 32-bit bloom filter. Seed: `0x25BBE08F`. Hash function: `xxh32(prefix + name, len, seed + index)`. Bit value `1` means NOT present (inverted logic). If bit is 0, xattr may exist.

---

## 10. Device Table (Multi-Device Support)

Source: `erofs_fs.h:42-50`, `super.c:136-261`

**Device slot** (128 bytes each):

| Offset | Size | Field        | Type     |
| ------ | ---- | ------------ | -------- |
| 0x00   | 64   | `tag`        | `[64]u8` |
| 0x40   | 4    | `blocks_lo`  | `u32le`  |
| 0x44   | 4    | `uniaddr_lo` | `u32le`  |
| 0x48   | 4    | `blocks_hi`  | `u32le`  |
| 0x4C   | 2    | `uniaddr_hi` | `u16le`  |
| 0x4E   | 50   | `reserved`   | `[50]u8` |

Device table starts at `devt_slotoff * 128` bytes from the image start.

For chunk-based files with `device_id != 0`, physical address is on device `device_id - 1`. `device_id_mask = roundup_pow_of_two(extra_devices + 1) - 1`.

For flat device mode, physical address is offset by the device's `uniaddr` blocks.

---

## 11. Filesystem Image Creation

### 11.1 Layout Process

**Phase 1 -- Scan and assign NIDs:**

1. Walk the source directory tree (DFS order).
2. Assign each inode a NID sequentially. Each inode consumes `ceil(total_meta / 32)` slots where `total_meta = inode_isize + xattr_isize + compression_index_size + inline_tail`.
3. Track hard links (same dev+ino) to share NIDs.

**Phase 2 -- Build directory blocks:**

1. For each directory, sort entries in strict lexicographic (byte-value) order.
2. Pack entries into blocks: entry array at start, names after.
3. `nameoff[0] = entry_count * 12`.
4. Subsequent: `nameoff[i] = nameoff[i-1] + len(name[i-1])`.
5. Null-terminate the last filename. Zero-pad remainder of block.

**Phase 3 -- Assign blocks and write data:**

Layout order (typical):

1. Block 0: First 1024 bytes (zero padding for boot sectors) + superblock + compression configs
2. Metadata area: inodes + inline data (starting at `meta_blkaddr`)
3. Data area: file data blocks, compressed pclusters

For each file:

- If `data_size <= available_inline_space`: use FLAT_INLINE
- Otherwise: use FLAT_PLAIN (or attempt compression)
- Record `startblk`

**Phase 4 -- Write superblock:**

1. Fill all superblock fields.
2. Compute CRC32-C checksum.
3. Write superblock at offset 1024.

### 11.2 NID Assignment

```
current_nid = 0
for each inode in DFS order:
    inode.nid = current_nid
    total_meta = inode_isize + xattr_isize + compression_index_size + inline_tail
    slots = ceil(total_meta / 32)
    current_nid += slots
```

Root directory NID stored in `rootnid_2b` (or `rootnid_8b` for 48-bit).

### 11.3 Inline Data Packing

Check feasibility for FLAT_INLINE:

```
iloc_blkoff = (meta_blkaddr * block_size + (nid << 5)) % block_size
meta_in_block = inode_isize + xattr_isize
tail_size = i_size % block_size

if iloc_blkoff + meta_in_block + tail_size <= block_size:
    use FLAT_INLINE
else:
    use FLAT_PLAIN
```

### 11.4 Building Compression Index

For compressed files:

1. Split file into lclusters of `block_size << h_clusterbits` bytes.
2. Compress each lcluster into a pcluster.
3. Record HEAD/NONHEAD status per lcluster.
4. Write map header at `ALIGN(inode_end, 8)`.
5. Write lcluster index entries (FULL or COMPACT format).
6. Write compressed pcluster data to the data area.

---

## 12. Validating Filesystem Image Correctness

### 12.1 Superblock Validation

Source: `super.c:263-369`

1. Read 4 bytes at offset 1024; verify `magic == 0xE0F5E1E2`.
2. Verify `blkszbits >= 9` and reasonable (e.g., <= 16).
3. Verify `dirblkbits == 0`.
4. Verify `feature_incompat & ~EROFS_ALL_FEATURE_INCOMPAT == 0`.
5. Verify `128 + sb_extslots * 16` fits within first block.
6. If `SB_CHKSUM` set: verify CRC32-C.
7. Resolve root NID; verify it points to a valid directory inode.

### 12.2 Inode Validation

Source: `inode.c:28-202`

1. Verify inode offset is within image.
2. Verify `i_format & ~EROFS_I_ALL == 0`.
3. Verify datalayout `< 5`.
4. Verify `i_mode` has valid file type bits.
5. Verify `i_size >= 0`.
6. For FLAT_INLINE: verify inline data does not cross block boundary.
7. For compressed: verify block counts are reasonable.

### 12.3 Directory Entry Validation

1. Verify `nameoff[0] >= 12` (at least one entry).
2. Verify `nameoff[0] < block_size`.
3. Verify `nameoff[i] < block_size` for all entries.
4. Verify `nameoff[i] < nameoff[i+1]` for non-last entries.
5. Verify filename lengths <= 255.
6. Verify `file_type` in range 0-7.

### 12.4 Compression Index Validation

1. Verify map header at correct alignment.
2. For each HEAD: verify `blkaddr` is within image.
3. For each NONHEAD: verify `delta[0] > 0` and chain terminates at HEAD.
4. Verify `clusterofs < lcluster_size` for HEADs.
5. Verify pcluster size <= 1 MiB.
6. Verify decompressed extent size <= 12 MiB.

---

## 13. Opening a Filesystem Image

### 13.1 Initialization Sequence

Source: `super.c:263-369`

1. Read 144 bytes at offset 1024; verify magic.
2. Parse superblock using `binary.Read` with `binary.LittleEndian`.
3. Validate `blkszbits`; compute `blockSize = 1 << blkszbits`.
4. Parse and validate feature flags.
5. Reject unknown incompatible features.
6. If `SB_CHKSUM`: read first block, verify CRC32-C.
7. Resolve root NID: if `48BIT` set, use `rootnid_8b`; else `rootnid_2b`.
8. If compression configs present: parse from `1024 + 128 + sb_extslots * 16`.
9. If `DEVICE_TABLE` set: parse device slots at `devt_slotoff * 128`.
10. Read and validate root inode.

### 13.2 In-Memory State

```go
type FS struct {
    r            io.ReaderAt
    blockSize    int
    blkSzBits    uint8
    metaBlkAddr  uint32
    xattrBlkAddr uint32
    rootNID      uint64
    packedNID    uint64
    epoch        int64
    fixedNsec    uint32
    featCompat   uint32
    featIncompat uint32
    // Compression
    availComprAlgs uint16
    lz4Cfg         lz4Config
    lzmaCfg        lzmaConfig
    deflateCfg     deflateConfig
    zstdCfg        zstdConfig
    // Device table
    devices      []deviceInfo
}
```

Inodes are parsed on demand by NID. Optional inode cache (LRU) recommended for performance.

---

## 14. Seeking and Rewinding Files

### 14.1 Uncompressed Files

For FLAT_PLAIN and FLAT_INLINE: `io.ReadSeeker` is trivial. Just update the logical offset. The next `Read` maps `la` to physical via the formulas in Section 5.

For FLAT_INLINE, seeking past the preceding-blocks boundary into the inline tail switches the read source from data area to metadata area.

### 14.2 Compressed Files

Each pcluster decompresses independently. No cross-pcluster state.

```go
type compressedFile struct {
    fs     *FS
    inode  *inode
    offset int64
    // Decompressed pcluster cache
    cacheStart int64   // logical offset of cached extent
    cacheEnd   int64
    cacheData  []byte
}
```

**Seek**: Just update logical offset. Invalidate cache if new offset is outside cached range. Do NOT decompress eagerly.

**Read**: If offset is within cached decompressed data, serve from cache. Otherwise:

1. Compute `lcn = offset >> lclusterbits`
2. Follow delta chain to HEAD
3. Read pcluster from disk
4. Decompress entire pcluster
5. Cache decompressed data with logical range
6. Copy requested bytes, advance offset

**Backward seeking**: Just update offset. Next Read decompresses the correct pcluster. No decompressor state to "rewind" because pclusters are independent.

### 14.3 Chunk-Based Files

Each chunk is independent:

```
chunk_number = offset >> chunk_bits
chunk_offset = offset & ((1 << chunk_bits) - 1)
```

Read chunk index, get physical block, read from `block_addr * block_size + chunk_offset`.

---

## 15. Automatic Compression at Creation Time

### 15.1 Strategy

1. **Try compression first**: For each file/lcluster, compress and measure output size.
2. **Fall back to flat**: If compressed size >= input size, store as PLAIN lcluster type (uncompressed but in the compressed index layout) or as FLAT_PLAIN.
3. **Use FLAT_INLINE** for small files and tails that fit inline.

### 15.2 Incompressibility Detection

Heuristics to skip compression entirely:

- File size is 0 or <= block_size: use FLAT_INLINE directly.
- File extension indicates already-compressed format (`.jpg`, `.png`, `.gif`, `.gz`, `.bz2`, `.xz`, `.zst`, `.zip`, `.mp4`, `.mp3`, `.webm`, `.webp`, `.avif`, `.woff2`).
- After compressing the first pcluster, if ratio > 0.95 (compressed >= 95% of original), mark file as incompressible and store flat.

### 15.3 Algorithm Selection

Default: LZ4 (fastest decompression, good for read-heavy workloads).

Dual-algorithm strategy:

- HEAD1 = LZ4 (fast path, algorithm bits 0-3)
- HEAD2 = ZSTD or DEFLATE (better ratio for cold data, algorithm bits 4-7)
- Per-lcluster: try LZ4 first; if ratio is poor, try HEAD2 algorithm

### 15.4 Cluster Sizing

- **Lcluster size**: Default `block_size` (`h_clusterbits = 0`). Larger lclusters (e.g., 2x or 4x block_size) improve compression ratio but increase decompression granularity for random access.
- **Pcluster size**: Default 1 block. With big pcluster enabled, can span multiple blocks for better ratio. Max `Z_EROFS_PCLUSTER_MAX_SIZE / block_size` blocks.
- **Trade-off**: Larger clusters = better compression, worse random access latency.

---

## 16. `fs.FS` Interface Design

### 16.1 Interface Mapping

| Go Interface     | EROFS Concept                                    |
| ---------------- | ------------------------------------------------ |
| `fs.FS`          | The filesystem image; `Open(name)` resolves path |
| `fs.File`        | An opened inode; `Read`, `Close`, `Stat`         |
| `fs.ReadDirFile` | An opened directory; `ReadDir`                   |
| `fs.StatFS`      | `Stat(name)` on the FS                           |
| `io.ReadSeeker`  | Random access within files                       |
| `io.ReaderAt`    | Concurrent random access                         |
| `fs.ReadLinkFS`  | `ReadLink(name)` for symlinks                    |

### 16.2 Type Hierarchy

```go
// FS implements fs.FS, fs.StatFS, fs.ReadLinkFS
type FS struct { ... }

func Open(r io.ReaderAt) (*FS, error)
func (f *FS) Open(name string) (fs.File, error)
func (f *FS) Stat(name string) (fs.FileInfo, error)
func (f *FS) ReadLink(name string) (string, error)

// file implements fs.File, io.ReadSeeker, io.ReaderAt
type file struct {
    fs     *FS
    inode  *inode
    name   string
    offset int64
    cache  *decompressedCache  // for compressed files
}

func (f *file) Stat() (fs.FileInfo, error)
func (f *file) Read(p []byte) (int, error)
func (f *file) Seek(offset int64, whence int) (int64, error)
func (f *file) ReadAt(p []byte, off int64) (int, error)
func (f *file) Close() error

// dir implements fs.ReadDirFile
type dir struct {
    fs      *FS
    inode   *inode
    name    string
    entries []fs.DirEntry  // lazily populated
    pos     int
}

func (d *dir) Stat() (fs.FileInfo, error)
func (d *dir) Read([]byte) (int, error)  // returns error
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error)
func (d *dir) Close() error
```

### 16.3 Path Resolution

`Open(name)`:

1. Validate with `fs.ValidPath`.
2. Start at root inode (root NID).
3. Split path by `/`.
4. For each component, directory lookup via binary search.
5. Follow symlinks (depth limit 40, matching Linux `MAXSYMLINKS`).
6. Return final inode as `file` or `dir`.

### 16.4 `fs.FileInfo` Mapping

```go
func modeFromErofs(imode uint16) fs.FileMode {
    mode := fs.FileMode(imode & 0o7777)
    switch imode & 0xF000 {
    case 0o040000: mode |= fs.ModeDir
    case 0o120000: mode |= fs.ModeSymlink
    case 0o010000: mode |= fs.ModeNamedPipe
    case 0o140000: mode |= fs.ModeSocket
    case 0o020000: mode |= fs.ModeDevice | fs.ModeCharDevice
    case 0o060000: mode |= fs.ModeDevice
    }
    return mode
}
```

### 16.5 Error Mapping

- File not found: `&fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}`
- Format errors: `fmt.Errorf("erofs: ...")`
- Corrupted data: `&fs.PathError{Op: "read", Path: name, Err: ErrCorrupted}`
- Unsupported features: `&fs.PathError{Op: "open", Path: name, Err: ErrNotSupported}`

---

## 17. Builder / Image Writer Interface

```go
type Builder struct {
    w           io.WriterAt
    blockSize   int
    blkSzBits   uint8
    compression CompressionConfig
    inodes      []*buildInode
    nextNID     uint64
    metaBlkAddr uint32
}

type CompressionConfig struct {
    Algorithm    int    // 0=LZ4, 1=LZMA, 2=DEFLATE, 3=ZSTD
    ClusterBits  int    // additional bits beyond blkszbits
    MaxPCluster  int    // max pcluster blocks (1 = no big pcluster)
    TryCompress  bool   // attempt compression, fall back to flat
}

func NewBuilder(w io.WriterAt, opts ...BuildOption) *Builder
func (b *Builder) AddFile(path string, info fs.FileInfo, r io.Reader) error
func (b *Builder) AddDir(path string, info fs.FileInfo) error
func (b *Builder) AddSymlink(path string, target string, info fs.FileInfo) error
func (b *Builder) AddFromFS(fsys fs.FS) error  // walk an fs.FS
func (b *Builder) Build() error                // finalize and write
```

`Build()` executes the phases from Section 11:

1. Assign NIDs (DFS order, sequential slots)
2. Lay out blocks (block 0 for SB, then metadata, then data)
3. Write metadata area
4. Write data area (compressed or flat)
5. Write superblock with checksum
6. Zero-fill first 1024 bytes

---

## 18. Implementation Phases

### Phase 1 -- Read-only fs.FS (MVP)

- Superblock parsing and validation
- Compact and extended inode reading
- FLAT_PLAIN and FLAT_INLINE data layouts
- Directory reading and binary search lookup
- Symlink resolution
- `fs.FS`, `fs.File`, `fs.ReadDirFile`, `fs.StatFS`

### Phase 2 -- Compressed file reading

- Compression map header parsing
- FULL lcluster index (layout 1)
- COMPACT lcluster index (layout 3)
- NONHEAD delta chain following
- LZ4 decompression
- `io.ReadSeeker` for compressed files
- Decompressed pcluster caching

### Phase 3 -- Additional decompressors

- ZSTD decompression
- DEFLATE decompression
- LZMA decompression
- SHIFTED and INTERLACED plain transforms

### Phase 4 -- Image creation

- Builder API
- NID assignment
- Directory block construction
- FLAT_PLAIN and FLAT_INLINE writing
- Superblock writing and checksumming

### Phase 5 -- Compression at creation time

- LZ4 compression with fallback
- Compression index building (FULL format)
- Compact index building
- Big pcluster support
- Incompressibility detection

### Phase 6 -- Advanced features

- Chunk-based file layout
- Multi-device support
- Xattr reading and writing
- Tail-packing and fragments
- 48-bit addressing

---

## 19. Verification Plan

### Reading

1. Create EROFS images using `mkfs.erofs` with known content.
2. Open with Go implementation, verify all files match original content byte-for-byte.
3. Test with compressed images (LZ4, ZSTD) and verify decompressed output matches.
4. Compare `fs.FileInfo` output against `stat` on mounted image.
5. Verify directory iteration order matches kernel driver.

### Writing

1. Build image with Go, mount with Linux kernel, verify all content.
2. Build image with Go, read back with Go, verify round-trip.
3. Compare Go-built images against `mkfs.erofs`-built images using `fsck.erofs`.
4. Verify CRC32-C checksum matches kernel computation.

### Critical Reference Files

- `kernel/fs/erofs/erofs_fs.h` -- All on-disk struct definitions
- `kernel/fs/erofs/super.c` -- Superblock reading, CRC32-C, compression config parsing
- `kernel/fs/erofs/data.c` -- Flat data mapping, inline data handling
- `kernel/fs/erofs/inode.c` -- Inode parsing, cross-block handling
- `kernel/fs/erofs/dir.c` -- Directory iteration
- `kernel/fs/erofs/namei.c` -- Binary search directory lookup
- `kernel/fs/erofs/zmap.c` -- Compressed file block mapping (FULL + COMPACT index)
- `kernel/fs/erofs/zdata.c` -- Compressed data I/O, pcluster decompression
- `kernel/fs/erofs/decompressor.c` -- Decompression dispatch
- `kernel/fs/erofs/decompressor_lzma.c` -- LZMA decompressor
- `kernel/fs/erofs/decompressor_deflate.c` -- DEFLATE decompressor
- `kernel/fs/erofs/decompressor_zstd.c` -- ZSTD decompressor
- `kernel/fs/erofs/xattr.c` -- Extended attribute handling
