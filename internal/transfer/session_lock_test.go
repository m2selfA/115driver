package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionLockRejectsConcurrentWriterAndMaintainsLease(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "locks", "aa", "id.lock")
	leasePath := filepath.Join(dir, "session", "lease.json")
	first, err := AcquireSessionLock(lockPath, leasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("lease was not written: %v", err)
	}
	second, err := AcquireSessionLock(lockPath, filepath.Join(dir, "other-lease.json"))
	if second != nil {
		_ = second.Close()
		t.Fatal("concurrent lock unexpectedly succeeded")
	}
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("expected ErrSessionLocked, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("lease survived lock close: %v", err)
	}
	third, err := AcquireSessionLock(lockPath, leasePath)
	if err != nil {
		t.Fatalf("lock could not be reacquired: %v", err)
	}
	_ = third.Close()
}
