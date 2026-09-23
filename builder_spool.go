package erofs

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// spool is a temp file that holds physical blocks between step 3b and step 7
// of Build. tryCompressFile appends the blocks of each candidate file to it:
// the compressed blocks, or the raw blocks of a file that stays flat. Step 7
// copies them to their final offsets in the image.
type spool struct {
	f   *os.File
	end int64 // offset of the next append
}

// append writes p at the end of the spool.
func (s *spool) append(p []byte) error {
	if _, err := s.f.WriteAt(p, s.end); err != nil {
		return fmt.Errorf("erofs: writing spool: %w", err)
	}
	s.end += int64(len(p))
	return nil
}

// openSpool returns the spool of the build, and creates it on first use.
func (b *Builder) openSpool() (*spool, error) {
	if b.spool != nil {
		return b.spool, nil
	}
	f, err := os.CreateTemp(b.spoolDir, "erofs-spool-*")
	if err != nil {
		return nil, fmt.Errorf("erofs: creating spool: %w", err)
	}
	b.spool = &spool{f: f}
	return b.spool, nil
}

// closeSpool closes and removes the spool, if there is one.
func (b *Builder) closeSpool() error {
	if b.spool == nil {
		return nil
	}
	f := b.spool.f
	b.spool = nil
	err := errors.Join(f.Close(), os.Remove(f.Name()))
	if err != nil {
		return fmt.Errorf("erofs: removing spool: %w", err)
	}
	return nil
}

// copyFromSpool copies n bytes from spool offset off to image offset dst,
// through the group buffer.
func (b *Builder) copyFromSpool(off, n, dst int64) error {
	buf := b.groupBuffer()
	for n > 0 {
		chunk := buf[:min(n, int64(len(buf)))]
		if _, err := b.spool.f.ReadAt(chunk, off); err != nil {
			return fmt.Errorf("erofs: reading spool: %w", err)
		}
		written, err := b.w.WriteAt(chunk, dst)
		if err != nil {
			return err
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		off += int64(len(chunk))
		dst += int64(len(chunk))
		n -= int64(len(chunk))
	}
	return nil
}

// groupBuffer returns the reused buffer for one pcluster group. It holds
// K+1 lclusters, because the last group of a file can absorb a trailing
// partial lcluster.
func (b *Builder) groupBuffer() []byte {
	size := (b.pclusterLclustersEff() + 1) * b.blockSize
	if len(b.groupBuf) != size {
		b.groupBuf = make([]byte, size)
	}
	return b.groupBuf
}
