package erofs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/Xe/erofs/internal/ondisk"
)

// DeviceInfo holds parsed information about an extra blob device.
type DeviceInfo struct {
	Tag     [64]byte
	Blocks  uint64 // total block count
	UniAddr uint64 // unified starting block address (for flat mode)
}

// parseDeviceTable reads extraDevices DeviceSlot entries starting at byteOffset.
func parseDeviceTable(r io.ReaderAt, extraDevices uint16, byteOffset int64) ([]DeviceInfo, error) {
	devs := make([]DeviceInfo, extraDevices)
	buf := make([]byte, ondisk.DevTSlotSize)

	for i := range devs {
		off := byteOffset + int64(i)*ondisk.DevTSlotSize
		if _, err := r.ReadAt(buf, off); err != nil {
			return nil, fmt.Errorf("erofs: reading device slot %d: %w", i, err)
		}

		var slot ondisk.DeviceSlot
		if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &slot); err != nil {
			return nil, fmt.Errorf("erofs: decoding device slot %d: %w", i, err)
		}

		devs[i] = DeviceInfo{
			Tag:     slot.Tag,
			Blocks:  uint64(slot.BlocksLo) | uint64(slot.BlocksHi)<<32,
			UniAddr: uint64(slot.UniAddrLo) | uint64(slot.UniAddrHi)<<32,
		}
	}
	return devs, nil
}

// computeDeviceIDMask returns the bitmask for valid device IDs.
// Matches the kernel's: roundup_pow_of_two(extra_devices + 1) - 1.
func computeDeviceIDMask(extraDevices uint16) uint16 {
	if extraDevices == 0 {
		return 0
	}
	n := uint32(extraDevices) + 1
	// Round up to next power of two.
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return uint16(n - 1)
}
