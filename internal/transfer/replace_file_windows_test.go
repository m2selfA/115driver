//go:build windows

package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReplaceDownloadedFileRetriesTransientWindowsShareDenial(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "new.json")
	to := filepath.Join(dir, "session.json")
	if err := os.WriteFile(from, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		toPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		// f58d0df retried for only ~355 ms total. Hold the destination long
		// enough to prove the extended bounded retry window, matching the kind
		// of longer non-share-delete hold observed from Windows background I/O.
		time.Sleep(750 * time.Millisecond)
		_ = windows.CloseHandle(handle)
		close(released)
	}()

	if err := replaceDownloadedFile(from, to); err != nil {
		<-released
		t.Fatalf("replace did not recover after transient sharing denial: %v", err)
	}
	<-released
	data, err := os.ReadFile(to)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("replacement contents = %q, want new", data)
	}
}
