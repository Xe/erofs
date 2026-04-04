package erofs

import (
	"bytes"
	"io"
	"io/fs"
	"testing"
	"time"
)

func TestBuilderFlatDeviceRoundTrip(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	blob0Data := bytes.Repeat([]byte{0xAA}, 4096)
	blob1Data := bytes.Repeat([]byte{0xBB}, 4096)

	imgBuf := newWriterAtBuffer(128 * 1024)
	b := NewBuilder(imgBuf, WithBlockSize(12), WithEpoch(epoch), WithChunkSize(12), WithFlatDevice())

	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})

	b.SetBlobInfo(1, BlobInfo{Blocks: 1})
	b.SetBlobInfo(2, BlobInfo{Blocks: 1})

	b.AddChunkedFile("/file0.bin", &staticFileInfo{
		name: "file0.bin", mode: 0o644, size: 4096, mod: epoch,
	}, []ChunkRef{{DeviceID: 1, BlkAddr: 0, Size: 4096}})

	b.AddChunkedFile("/file1.bin", &staticFileInfo{
		name: "file1.bin", mode: 0o644, size: 4096, mod: epoch,
	}, []ChunkRef{{DeviceID: 2, BlkAddr: 0, Size: 4096}})

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	img := imgBuf.Bytes()

	// Open to read device table and verify flat mode.
	fsys, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !fsys.flatDev {
		t.Fatal("expected flatDev to be true")
	}
	if len(fsys.devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(fsys.devices))
	}

	// Build the composed flat image: [image | blob0 | blob1]
	totalSize := len(img)
	for _, dev := range fsys.devices {
		end := int((dev.UniAddr + dev.Blocks)) << 12
		if end > totalSize {
			totalSize = end
		}
	}
	flat := make([]byte, totalSize)
	copy(flat, img)
	copy(flat[int(fsys.devices[0].UniAddr)<<12:], blob0Data)
	copy(flat[int(fsys.devices[1].UniAddr)<<12:], blob1Data)

	fsys2, err := Open(bytes.NewReader(flat))
	if err != nil {
		t.Fatalf("Open flat: %v", err)
	}

	data0, err := fs.ReadFile(fsys2, "file0.bin")
	if err != nil {
		t.Fatalf("ReadFile file0.bin: %v", err)
	}
	if !bytes.Equal(data0, blob0Data) {
		t.Error("file0.bin content mismatch")
	}

	data1, err := fs.ReadFile(fsys2, "file1.bin")
	if err != nil {
		t.Fatalf("ReadFile file1.bin: %v", err)
	}
	if !bytes.Equal(data1, blob1Data) {
		t.Error("file1.bin content mismatch")
	}
}

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

func TestBuilderMultiChunkFile(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	imgBuf := newWriterAtBuffer(64 * 1024)

	// 2 chunks of 4096 bytes each, on different devices.
	chunk0 := bytes.Repeat([]byte{0x11}, 4096)
	chunk1 := bytes.Repeat([]byte{0x22}, 4096)
	blob0Buf := newWriterAtBuffer(8192)
	blob0Buf.WriteAt(chunk0, 0)
	blob1Buf := newWriterAtBuffer(8192)
	blob1Buf.WriteAt(chunk1, 0)

	b := NewBuilder(imgBuf, WithBlockSize(12), WithEpoch(epoch), WithChunkSize(12))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	b.SetBlobInfo(1, BlobInfo{Blocks: 1})
	b.SetBlobInfo(2, BlobInfo{Blocks: 1})

	b.AddChunkedFile("/split.bin", &staticFileInfo{
		name: "split.bin", mode: 0o644, size: 8192, mod: epoch,
	}, []ChunkRef{
		{DeviceID: 1, BlkAddr: 0, Size: 4096},
		{DeviceID: 2, BlkAddr: 0, Size: 4096},
	})

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	fsys, err := OpenMultiBlob(
		bytes.NewReader(imgBuf.Bytes()),
		[]io.ReaderAt{bytes.NewReader(blob0Buf.Bytes()), bytes.NewReader(blob1Buf.Bytes())},
	)
	if err != nil {
		t.Fatalf("OpenMultiBlob: %v", err)
	}

	data, err := fs.ReadFile(fsys, "split.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := append(chunk0, chunk1...)
	if !bytes.Equal(data, want) {
		t.Errorf("content mismatch: got %d bytes", len(data))
	}
}

func TestBuilderChunkWithHole(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	imgBuf := newWriterAtBuffer(64 * 1024)

	blobData := bytes.Repeat([]byte{0xFF}, 4096)
	blobBuf := newWriterAtBuffer(8192)
	blobBuf.WriteAt(blobData, 0)

	b := NewBuilder(imgBuf, WithBlockSize(12), WithEpoch(epoch), WithChunkSize(12))
	b.AddDir("/", &staticFileInfo{name: "/", mode: fs.ModeDir | 0o755, mod: epoch})
	b.SetBlobInfo(1, BlobInfo{Blocks: 1})

	// 2 chunks: first is a hole (NullAddr), second has data.
	b.AddChunkedFile("/sparse.bin", &staticFileInfo{
		name: "sparse.bin", mode: 0o644, size: 8192, mod: epoch,
	}, []ChunkRef{
		{DeviceID: 0, BlkAddr: 0xFFFFFFFF, Size: 4096}, // hole
		{DeviceID: 1, BlkAddr: 0, Size: 4096},
	})

	if err := b.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	fsys, err := OpenMultiBlob(
		bytes.NewReader(imgBuf.Bytes()),
		[]io.ReaderAt{bytes.NewReader(blobBuf.Bytes())},
	)
	if err != nil {
		t.Fatalf("OpenMultiBlob: %v", err)
	}

	data, err := fs.ReadFile(fsys, "sparse.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// First 4096 bytes should be zeros (hole), next 4096 should be 0xFF.
	for i := 0; i < 4096; i++ {
		if data[i] != 0 {
			t.Fatalf("byte %d in hole = %02x, want 0", i, data[i])
		}
	}
	for i := 4096; i < 8192; i++ {
		if data[i] != 0xFF {
			t.Fatalf("byte %d in data = %02x, want FF", i, data[i])
		}
	}
}
