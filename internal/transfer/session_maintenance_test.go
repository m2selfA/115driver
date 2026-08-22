package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpportunisticGCProtectsCurrentLockedSessionAndHonorsInterval(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	location, _ := createAdminTestSession(t, store, "current.bin", now.Add(-40*24*time.Hour))
	lock, err := AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := store.RunOpportunisticGC(24*time.Hour, SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	var sawLocked bool
	for _, action := range actions {
		if action.SessionID == location.ID && action.Action == "skip" && action.Reason == "locked" {
			sawLocked = true
		}
	}
	if !sawLocked {
		_ = lock.Close()
		t.Fatalf("current old session was not protected by its lock: %#v", actions)
	}
	if _, err := os.Stat(location.Dir); err != nil {
		_ = lock.Close()
		t.Fatalf("current locked session was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "maintenance.json")); err != nil {
		_ = lock.Close()
		t.Fatalf("maintenance timestamp was not written: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	actions, err = store.RunOpportunisticGC(24*time.Hour, SessionGCOptions{Now: now.Add(time.Hour), Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("GC interval was ignored: %#v", actions)
	}
	if _, err := os.Stat(location.Dir); err != nil {
		t.Fatalf("interval-skipped GC mutated session: %v", err)
	}
	actions, err = store.RunOpportunisticGC(24*time.Hour, SessionGCOptions{Now: now.Add(25 * time.Hour), Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(location.Dir); !os.IsNotExist(err) {
		t.Fatalf("expired unlocked session was not trashed: %v actions=%#v", err, actions)
	}
}
