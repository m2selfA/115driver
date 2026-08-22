package transfer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivateFileAtomicCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if err := WritePrivateFileAtomic(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFileAtomic(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("atomic private file contents: got %q want %q", data, "second")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("atomic private file permissions are too broad: %v", info.Mode().Perm())
		}
	}
}
