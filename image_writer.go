package erofs

import "io"

// imageWriter wraps the output of a Builder. It keeps a copy of the first
// block, so that the superblock checksum does not need to read the output
// back, and it records the end of the highest write, so that Build can pad the
// image to a whole number of blocks.
type imageWriter struct {
	w     io.WriterAt
	first []byte // copy of block 0
	end   int64  // one past the highest byte written
}

func newImageWriter(w io.WriterAt, blockSize int) *imageWriter {
	return &imageWriter{w: w, first: make([]byte, blockSize)}
}

func (iw *imageWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := iw.w.WriteAt(p, off)
	if off < int64(len(iw.first)) && n > 0 {
		copy(iw.first[off:], p[:n])
	}
	if end := off + int64(n); end > iw.end {
		iw.end = end
	}
	return n, err
}

// padTo writes zeros from the end of the highest write up to size.
func (iw *imageWriter) padTo(size int64) error {
	if iw.end >= size {
		return nil
	}
	zeros := make([]byte, min(size-iw.end, 64<<10))
	for iw.end < size {
		chunk := zeros[:min(size-iw.end, int64(len(zeros)))]
		n, err := iw.WriteAt(chunk, iw.end)
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
	}
	return nil
}
