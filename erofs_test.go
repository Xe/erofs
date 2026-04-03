package erofs

import (
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

const testImage = "testdata/toybox.img"

func openTestFS(t *testing.T) *FS {
	t.Helper()
	f, err := os.Open(testImage)
	if err != nil {
		t.Skipf("test image not available: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	fsys, err := Open(f)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return fsys
}

func TestOpen(t *testing.T) {
	fsys := openTestFS(t)

	if fsys.blockSize != 4096 {
		t.Errorf("blockSize = %d, want 4096", fsys.blockSize)
	}
	if fsys.rootNID != 36 {
		t.Errorf("rootNID = %d, want 36", fsys.rootNID)
	}
}

func TestOpenRoot(t *testing.T) {
	fsys := openTestFS(t)

	f, err := fsys.Open(".")
	if err != nil {
		t.Fatalf("Open(.): %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("root is not a directory")
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("root mode = %o, want 0755", info.Mode().Perm())
	}
}

func TestReadDir(t *testing.T) {
	fsys := openTestFS(t)

	f, err := fsys.Open(".")
	if err != nil {
		t.Fatalf("Open(.): %v", err)
	}
	defer f.Close()

	dirFile, ok := f.(fs.ReadDirFile)
	if !ok {
		t.Fatal("root file does not implement ReadDirFile")
	}

	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// The toybox image root should have these entries (excluding . and ..)
	expected := map[string]bool{
		"bin": true, "dev": true, "etc": true, "home": true,
		"init": true, "lib": true, "mnt": true, "proc": true,
		"root": true, "run": true, "sbin": true, "sys": true,
		"tmp": true, "usr": true, "var": true,
	}

	found := make(map[string]bool)
	for _, e := range entries {
		found[e.Name()] = true
	}

	for name := range expected {
		if !found[name] {
			t.Errorf("missing entry %q", name)
		}
	}
}

func TestReadFile(t *testing.T) {
	fsys := openTestFS(t)

	data, err := fs.ReadFile(fsys, "etc/resolv.conf")
	if err != nil {
		t.Fatalf("ReadFile(etc/resolv.conf): %v", err)
	}

	want := "nameserver 8.8.8.8\n"
	if string(data) != want {
		t.Errorf("content = %q, want %q", string(data), want)
	}
}

func TestReadPasswd(t *testing.T) {
	fsys := openTestFS(t)

	data, err := fs.ReadFile(fsys, "etc/passwd")
	if err != nil {
		t.Fatalf("ReadFile(etc/passwd): %v", err)
	}

	if !strings.HasPrefix(string(data), "root:x:0:0:root:") {
		t.Errorf("passwd doesn't start with expected content: %q", string(data)[:50])
	}
	if len(data) != 121 {
		t.Errorf("passwd size = %d, want 121", len(data))
	}
}

func TestReadLink(t *testing.T) {
	fsys := openTestFS(t)

	target, err := fsys.ReadLink("bin")
	if err != nil {
		t.Fatalf("ReadLink(bin): %v", err)
	}
	if target != "usr/bin" {
		t.Errorf("bin target = %q, want %q", target, "usr/bin")
	}
}

func TestStat(t *testing.T) {
	fsys := openTestFS(t)

	info, err := fsys.Stat("etc")
	if err != nil {
		t.Fatalf("Stat(etc): %v", err)
	}
	if !info.IsDir() {
		t.Error("etc is not a directory")
	}
	if info.Name() != "etc" {
		t.Errorf("name = %q, want %q", info.Name(), "etc")
	}
}

func TestStatFile(t *testing.T) {
	fsys := openTestFS(t)

	info, err := fsys.Stat("etc/resolv.conf")
	if err != nil {
		t.Fatalf("Stat(etc/resolv.conf): %v", err)
	}
	if info.IsDir() {
		t.Error("resolv.conf is a directory")
	}
	if info.Size() != 19 {
		t.Errorf("size = %d, want 19", info.Size())
	}
}

func TestReadDirEtc(t *testing.T) {
	fsys := openTestFS(t)

	entries, err := fs.ReadDir(fsys, "etc")
	if err != nil {
		t.Fatalf("ReadDir(etc): %v", err)
	}

	expected := map[string]bool{
		"group": true, "os-release": true, "passwd": true,
		"rc": true, "resolv.conf": true,
	}

	found := make(map[string]bool)
	for _, e := range entries {
		found[e.Name()] = true
	}

	for name := range expected {
		if !found[name] {
			t.Errorf("missing entry %q", name)
		}
	}
}

func TestSymlinkFollowing(t *testing.T) {
	fsys := openTestFS(t)

	// bin -> usr/bin, which should be a directory
	info, err := fsys.Stat("bin")
	if err != nil {
		t.Fatalf("Stat(bin): %v", err)
	}
	// bin is a symlink to usr/bin, Stat follows symlinks
	if !info.IsDir() {
		t.Error("bin (via symlink) should be a directory")
	}
}

func TestFileSeek(t *testing.T) {
	fsys := openTestFS(t)

	// etc/passwd is 121 bytes:
	// "root:x:0:0:root:/root:/bin/sh\n
	//  guest:x:500:500:guest:/home/guest:/bin/sh\n
	//  nobody:x:65534:65534:nobody:/proc/self:/dev/null\n"
	const passwdSize = 121

	openPasswd := func(t *testing.T) (io.ReadSeeker, func()) {
		t.Helper()
		f, err := fsys.Open("etc/passwd")
		if err != nil {
			t.Fatalf("Open(etc/passwd): %v", err)
		}
		return f.(io.ReadSeeker), func() { f.Close() }
	}

	t.Run("SeekStart", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		buf := make([]byte, 4)
		s.Read(buf) // advance to offset 4

		pos, err := s.Seek(0, io.SeekStart)
		if err != nil {
			t.Fatalf("Seek(0, Start): %v", err)
		}
		if pos != 0 {
			t.Errorf("pos = %d, want 0", pos)
		}

		n, err := s.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read: %v", err)
		}
		if string(buf[:n]) != "root" {
			t.Errorf("got %q, want %q", string(buf[:n]), "root")
		}
	})

	t.Run("SeekStartMiddle", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		pos, err := s.Seek(30, io.SeekStart)
		if err != nil {
			t.Fatalf("Seek(30, Start): %v", err)
		}
		if pos != 30 {
			t.Errorf("pos = %d, want 30", pos)
		}

		buf := make([]byte, 4)
		n, err := s.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read: %v", err)
		}
		// Byte 30 is start of "guest:x:500:..."
		if string(buf[:n]) != "gues" {
			t.Errorf("got %q, want %q", string(buf[:n]), "gues")
		}
	})

	t.Run("SeekCurrentForward", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		buf := make([]byte, 4)
		s.Read(buf) // now at offset 4

		pos, err := s.Seek(1, io.SeekCurrent)
		if err != nil {
			t.Fatalf("Seek(1, Current): %v", err)
		}
		if pos != 5 {
			t.Errorf("pos = %d, want 5", pos)
		}

		n, err := s.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read: %v", err)
		}
		if string(buf[:n]) != "x:0:" {
			t.Errorf("got %q, want %q", string(buf[:n]), "x:0:")
		}
	})

	t.Run("SeekCurrentBackward", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		s.Seek(50, io.SeekStart)
		pos, err := s.Seek(-10, io.SeekCurrent)
		if err != nil {
			t.Fatalf("Seek(-10, Current): %v", err)
		}
		if pos != 40 {
			t.Errorf("pos = %d, want 40", pos)
		}
	})

	t.Run("SeekEndNegative", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		pos, err := s.Seek(-5, io.SeekEnd)
		if err != nil {
			t.Fatalf("Seek(-5, End): %v", err)
		}
		if pos != passwdSize-5 {
			t.Errorf("pos = %d, want %d", pos, passwdSize-5)
		}

		buf := make([]byte, 10)
		n, err := s.Read(buf)
		if n != 5 {
			t.Errorf("read %d bytes, want 5", n)
		}
		if string(buf[:n]) != "null\n" {
			t.Errorf("got %q, want %q", string(buf[:n]), "null\n")
		}
		if err != io.EOF {
			t.Errorf("err = %v, want EOF", err)
		}
	})

	t.Run("SeekEndExact", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		pos, err := s.Seek(0, io.SeekEnd)
		if err != nil {
			t.Fatalf("Seek(0, End): %v", err)
		}
		if pos != passwdSize {
			t.Errorf("pos = %d, want %d", pos, passwdSize)
		}

		buf := make([]byte, 4)
		n, err := s.Read(buf)
		if n != 0 || err != io.EOF {
			t.Errorf("Read at EOF: n=%d, err=%v, want 0, EOF", n, err)
		}
	})

	t.Run("SeekNegativeOffset", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		_, err := s.Seek(-1, io.SeekStart)
		if err == nil {
			t.Error("Seek(-1, Start) should fail")
		}
	})

	t.Run("SeekPastEOF", func(t *testing.T) {
		s, close := openPasswd(t)
		defer close()

		pos, err := s.Seek(passwdSize+100, io.SeekStart)
		if err != nil {
			t.Fatalf("Seek past EOF: %v", err)
		}
		if pos != passwdSize+100 {
			t.Errorf("pos = %d, want %d", pos, passwdSize+100)
		}

		buf := make([]byte, 4)
		n, err := s.Read(buf)
		if n != 0 || err != io.EOF {
			t.Errorf("Read past EOF: n=%d, err=%v, want 0, EOF", n, err)
		}
	})

	t.Run("ReadAtIndependent", func(t *testing.T) {
		f, err := fsys.Open("etc/passwd")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()

		seeker := f.(io.ReadSeeker)
		rat := f.(io.ReaderAt)

		// Move seek position somewhere arbitrary.
		seeker.Seek(50, io.SeekStart)

		// ReadAt at offset 0 should still work.
		buf := make([]byte, 4)
		n, err := rat.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(0): %v", err)
		}
		if string(buf[:n]) != "root" {
			t.Errorf("ReadAt(0) = %q, want %q", string(buf[:n]), "root")
		}

		// ReadAt near end.
		n, err = rat.ReadAt(buf, passwdSize-4)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(end-4): %v", err)
		}
		if string(buf[:n]) != "ull\n" {
			t.Errorf("ReadAt(end-4) = %q, want %q", string(buf[:n]), "ull\n")
		}

		// Seek position should be unchanged.
		pos, _ := seeker.Seek(0, io.SeekCurrent)
		if pos != 50 {
			t.Errorf("seek pos after ReadAt = %d, want 50", pos)
		}
	})
}

func TestLstat(t *testing.T) {
	fsys := openTestFS(t)

	info, err := fsys.Lstat("bin")
	if err != nil {
		t.Fatalf("Lstat(bin): %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Error("Lstat(bin) should show symlink mode")
	}
}

func TestNotExist(t *testing.T) {
	fsys := openTestFS(t)

	_, err := fsys.Open("nonexistent")
	if err == nil {
		t.Fatal("Open(nonexistent) should fail")
	}
}

func TestFSImplementsInterfaces(t *testing.T) {
	fsys := openTestFS(t)
	var _ fs.FS = fsys
	var _ fs.StatFS = fsys
}

func TestWalkDir(t *testing.T) {
	fsys := openTestFS(t)

	count := 0
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if count < 10 {
		t.Errorf("WalkDir visited only %d entries, expected more", count)
	}
	t.Logf("WalkDir visited %d entries", count)
}
