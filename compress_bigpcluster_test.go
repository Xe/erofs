package erofs

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"testing"
)

// TestReaderBigPClusterFixtures reads real mkfs.erofs big-pcluster images
// (FULL/legacy index) to verify the reader's decode path against reference
// tooling: a single-physical-block pcluster (C=1, partial-tail marker) and a
// multi-block pcluster (C=11, exercising the D0_CBLKCNT block count).
func TestReaderBigPClusterFixtures(t *testing.T) {
	cases := []struct{ img, name, src string }{
		{"testdata/bigpcluster-lz4-c1.img", "big.txt", "testdata/bigpcluster-lz4-c1.txt"},
		{"testdata/bigpcluster-lz4-c11.img", "semi.bin", "testdata/bigpcluster-lz4-c11.bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(tc.img)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			fsys, err := Open(f)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := fs.ReadFile(fsys, tc.name)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			want, err := os.ReadFile(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: content mismatch (got %d bytes, want %d)", tc.name, len(got), len(want))
			}
		})
	}
}

// TestReaderBigPClusterRandomAccess verifies random-access reads, including
// offsets that land in the partial-tail marker lcluster (which previously
// looped forever).
func TestReaderBigPClusterRandomAccess(t *testing.T) {
	f, err := os.Open("testdata/bigpcluster-lz4-c1.img")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fsys, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/bigpcluster-lz4-c1.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 5000, 100000, 131072, 134000, 134999} {
		file, err := fsys.Open("big.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.(io.Seeker).Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 200)
		n, _ := io.ReadFull(file, buf) // short read near EOF is fine
		file.Close()
		if n == 0 || !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
			t.Fatalf("offset %d: mismatch (n=%d)", off, n)
		}
	}
}
