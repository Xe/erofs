package erofs

import (
	"bytes"
	"io"
	"io/fs"
	"testing"
	"time"
)

func TestBuilderChunkBasedRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	imgBuf := newWriterAtBuffer(64 * 1024)

	// Blob data: one 4096-byte chunk of known content.
	blobData := make([]byte, 4096)
	for i := range blobData {
		blobData[i] = byte(i % 251) // prime modulus for pattern
	}
	blobBuf := newWriterAtBuffer(8192)
	blobBuf.WriteAt(blobData, 0)

	b := NewBuilder(imgBuf, WithBlockSize(12), WithEpoch(epoch), WithChunkSize(12))

	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	// Add a file whose data lives in blob 0.
	b.AddChunkedFile("/data.bin", &staticFileInfo{
		name: "data.bin",
		mode: 0o644,
		size: int64(len(blobData)),
		mod:  epoch,
	}, []ChunkRef{
		{DeviceID: 1, BlkAddr: 0, Size: uint32(len(blobData))},
	})

	b.SetBlobInfo(1, BlobInfo{Blocks: 1})

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Open with the blob.
	fsys, err := OpenMultiBlob(
		bytes.NewReader(imgBuf.Bytes()),
		[]io.ReaderAt{bytes.NewReader(blobBuf.Bytes())},
	)
	if err != nil {
		t.Fatalf("OpenMultiBlob: %v", err)
	}

	data, err := fs.ReadFile(fsys, "data.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, blobData) {
		t.Errorf("content mismatch: got %d bytes", len(data))
	}
}
