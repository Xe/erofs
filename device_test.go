package erofs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Xe/erofs/internal/ondisk"
)

func TestParseDeviceTable(t *testing.T) {
	// Build a fake device table with 2 entries at offset 256.
	buf := make([]byte, 512)
	slot0 := ondisk.DeviceSlot{
		BlocksLo:  100,
		UniAddrLo: 0,
	}
	copy(slot0.Tag[:], "blob0")
	slot1 := ondisk.DeviceSlot{
		BlocksLo:  200,
		UniAddrLo: 100,
	}
	copy(slot1.Tag[:], "blob1")

	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, &slot0)
	copy(buf[256:], b.Bytes())
	b.Reset()
	binary.Write(&b, binary.LittleEndian, &slot1)
	copy(buf[256+128:], b.Bytes())

	r := bytes.NewReader(buf)
	devs, err := parseDeviceTable(r, 2, 256)
	if err != nil {
		t.Fatalf("parseDeviceTable: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2", len(devs))
	}
	if devs[0].Blocks != 100 {
		t.Errorf("dev[0].Blocks = %d, want 100", devs[0].Blocks)
	}
	if devs[1].UniAddr != 100 {
		t.Errorf("dev[1].UniAddr = %d, want 100", devs[1].UniAddr)
	}
	if string(bytes.TrimRight(devs[0].Tag[:], "\x00")) != "blob0" {
		t.Errorf("dev[0].Tag = %q, want blob0", devs[0].Tag)
	}
}

func TestDeviceIDMask(t *testing.T) {
	tests := []struct {
		extraDevices uint16
		wantMask     uint16
	}{
		{0, 0},
		{1, 1},
		{2, 3},
		{3, 3},
		{4, 7},
		{5, 7},
	}
	for _, tt := range tests {
		got := computeDeviceIDMask(tt.extraDevices)
		if got != tt.wantMask {
			t.Errorf("computeDeviceIDMask(%d) = %d, want %d", tt.extraDevices, got, tt.wantMask)
		}
	}
}
