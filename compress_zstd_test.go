package erofs

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestZstdDecompressToleratesPadding(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	orig := bytes.Repeat([]byte("erofs zstd pcluster padding\n"), 64)
	frame := enc.EncodeAll(orig, nil)

	// Simulate an EROFS pcluster: one block, zstd frame at the front,
	// remainder zero-padded -- exactly what the builder writes.
	block := make([]byte, 4096)
	copy(block, frame)

	dst := make([]byte, len(orig))
	n, err := zstdDecompress(block, dst)
	if err != nil {
		t.Fatalf("zstdDecompress: %v", err)
	}
	if n != len(orig) {
		t.Fatalf("n = %d, want %d", n, len(orig))
	}
	if !bytes.Equal(dst[:n], orig) {
		t.Fatalf("decompressed content mismatch")
	}
}
