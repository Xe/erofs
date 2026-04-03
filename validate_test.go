package erofs

import (
	"os"
	"testing"
)

func TestValidate(t *testing.T) {
	f, err := os.Open(testImage)
	if err != nil {
		t.Skipf("test image not available: %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	result, err := Validate(f, info.Size())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, e := range result.Errors {
		t.Errorf("validation error: %s", e.Error())
	}
	for _, w := range result.Warnings {
		t.Logf("validation warning: %s", w.Error())
	}

	if len(result.Errors) > 0 {
		t.Fatalf("expected 0 validation errors, got %d", len(result.Errors))
	}

	t.Logf("inodes=%d files=%d dirs=%d symlinks=%d",
		result.InodeCount, result.FileCount, result.DirCount, result.SymlinkCount)

	if result.InodeCount == 0 {
		t.Error("expected at least 1 inode")
	}
	if result.DirCount == 0 {
		t.Error("expected at least 1 directory")
	}
}
