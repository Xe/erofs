package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/Xe/erofs/internal/ondisk"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// readSuperBlock reads and validates the EROFS superblock from r.
func readSuperBlock(r io.ReaderAt) (*ondisk.SuperBlock, error) {
	var sb ondisk.SuperBlock
	buf := make([]byte, 144)
	if _, err := r.ReadAt(buf, ondisk.SuperOffset); err != nil {
		return nil, fmt.Errorf("erofs: reading superblock: %w", err)
	}
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &sb); err != nil {
		return nil, fmt.Errorf("erofs: decoding superblock: %w", err)
	}
	if sb.Magic != ondisk.SuperMagic {
		return nil, fmt.Errorf("erofs: bad magic 0x%08X, want 0x%08X", sb.Magic, ondisk.SuperMagic)
	}
	if sb.BlkSzBits < 9 || sb.BlkSzBits > 16 {
		return nil, fmt.Errorf("erofs: invalid block size bits %d", sb.BlkSzBits)
	}
	if sb.FeatureIncompat & ^uint32(ondisk.AllFeatureIncompat) != 0 {
		return nil, fmt.Errorf("erofs: unsupported incompatible features 0x%08X", sb.FeatureIncompat & ^uint32(ondisk.AllFeatureIncompat))
	}
	return &sb, nil
}

// verifySuperBlockChecksum verifies the CRC32-C checksum of the superblock.
func verifySuperBlockChecksum(r io.ReaderAt, sb *ondisk.SuperBlock) error {
	if sb.FeatureCompat&ondisk.FeatureCompatSBChksum == 0 {
		return nil // no checksum to verify
	}
	blockSize := 1 << sb.BlkSzBits
	block := make([]byte, blockSize)
	if _, err := r.ReadAt(block, 0); err != nil {
		return fmt.Errorf("erofs: reading first block for checksum: %w", err)
	}
	// CRC input: bytes after the checksum field to end of first block.
	// Checksum field is at offset 1024+4, size 4. So data starts at 1024+8.
	start := ondisk.SuperOffset + 8
	if start >= blockSize {
		return fmt.Errorf("erofs: block size %d too small for superblock", blockSize)
	}
	// The kernel's crc32c(seed, data, len) uses the seed as raw CRC register state.
	// Go's crc32.Update does pre/post XOR on the CRC register, so to match:
	// kernel_crc32c(seed, data) = ^crc32.Update(^seed, table, data)
	crc := ^crc32.Update(^uint32(ondisk.SBChecksumSeed), crc32cTable, block[start:blockSize])
	if crc != sb.Checksum {
		return fmt.Errorf("erofs: superblock checksum mismatch: computed 0x%08X, stored 0x%08X", crc, sb.Checksum)
	}
	return nil
}
