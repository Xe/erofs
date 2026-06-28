package ondisk

import (
	"encoding/binary"
	"testing"
)

func TestSuperBlockSize(t *testing.T) {
	if got := binary.Size(SuperBlock{}); got != SuperBlockSize {
		t.Fatalf("binary.Size(SuperBlock{}) = %d, want SuperBlockSize = %d", got, SuperBlockSize)
	}
}
