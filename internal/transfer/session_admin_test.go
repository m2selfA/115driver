package transfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func createAdminTestSession(t *testing.T, store SessionStore, name string, lastUsed time.Time) (SessionLocation, SessionManifest) {
	t.Helper()
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "file", scope, filepath.Join(t.TempDir(), name), "/Remote", "multipart", "single-file")
	if err != nil {
		t.Fatal(err)
	}
	location, manifest, err := store.Open(identity, name, 42)
	if err != nil {
		t.Fatal(err)
	}
	manifest.LastUsedAt = lastUsed
	manifest.UpdatedAt = lastUsed
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	return location, manifest
}

func TestSessionAdminListInspectAndTrash(t *testing.T) {
	store := SessionStore{Root: t.TempDir()}
	location, manifest := createAdminTestSession(t, store, "a.bin", time.Now().UTC())
	entries, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != manifest.SessionID || entries[0].InUse {
		t.Fatalf("unexpected entries: %#v", entries)
	}
	entry, err := store.InspectSession(manifest.SessionID[:8])
	if err != nil || entry.ID != manifest.SessionID {
		t.Fatalf("inspect failed: %#v err=%v", entry, err)
	}
	trashPath, err := store.TrashSession(manifest.SessionID[:8], "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(location.Dir); !os.IsNotExist(err) {
		t.Fatalf("session was not moved to trash: %v", err)
	}
	if info, err := os.Stat(trashPath); err != nil || !info.IsDir() {
		t.Fatalf("trash target missing: %v", err)
	}
}

func TestSessionAdminTrashHookFailurePreservesSessionWhileLocked(t *testing.T) {
	store := SessionStore{Root: t.TempDir()}
	location, manifest := createAdminTestSession(t, store, "abort.bin", time.Now().UTC())
	abortErr := errors.New("remote abort failed")
	_, err := store.TrashSessionWithHook(manifest.SessionID, "manual", func(entry SessionEntry) error {
		if entry.ID != manifest.SessionID {
			t.Fatalf("hook received wrong session: %#v", entry)
		}
		inUse, lockErr := SessionLockInUse(location.LockPath)
		if lockErr != nil || !inUse {
			t.Fatalf("session lock was not held during hook: inUse=%v err=%v", inUse, lockErr)
		}
		return abortErr
	})
	if !errors.Is(err, abortErr) {
		t.Fatalf("hook failure was not returned: %v", err)
	}
	if _, err := os.Stat(location.Dir); err != nil {
		t.Fatalf("session was trashed despite remote abort failure: %v", err)
	}
}

func TestSessionAdminTrashRefusesLockedSession(t *testing.T) {
	store := SessionStore{Root: t.TempDir()}
	location, manifest := createAdminTestSession(t, store, "locked.bin", time.Now().UTC())
	lock, err := AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := store.TrashSession(manifest.SessionID, "manual"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("expected locked session refusal, got %v", err)
	}
}

func TestSessionAdminListFindsMissingManifestDirectoryAndGCQuarantinesIt(t *testing.T) {
	store := SessionStore{Root: t.TempDir()}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := filepath.Join(store.Root, "v2", "upload", "file", id[:2], "orphan--"+id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != id || !entries[0].Corrupt || entries[0].Error != "session manifest is missing" {
		t.Fatalf("missing-manifest session was not discovered as corrupt: %#v", entries)
	}
	actions, err := store.GC(SessionGCOptions{Now: time.Now().UTC(), Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("missing-manifest session was not quarantined: %v actions=%#v", err, actions)
	}
}

func TestSessionAdminMarksActiveSessionStaleAfterSevenDays(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	_, staleManifest := createAdminTestSession(t, store, "stale.bin", now.Add(-8*24*time.Hour))
	_, freshManifest := createAdminTestSession(t, store, "fresh.bin", now.Add(-6*24*time.Hour))
	entries, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]SessionEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if !byID[staleManifest.SessionID].Stale {
		t.Fatalf("old active session was not marked stale: %#v", byID[staleManifest.SessionID])
	}
	if byID[freshManifest.SessionID].Stale {
		t.Fatalf("fresh active session was marked stale: %#v", byID[freshManifest.SessionID])
	}
}

func TestSessionAdminRevalidatesAgeAfterCandidateSelection(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	location, manifest := createAdminTestSession(t, store, "race.bin", now.Add(-40*24*time.Hour))
	entries, err := store.ListSessions()
	if err != nil || len(entries) != 1 {
		t.Fatalf("list candidate: entries=%#v err=%v", entries, err)
	}
	manifest.LastUsedAt = now
	manifest.UpdatedAt = now
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	_, ok, err := store.revalidateTrashCandidate(entries[0], "expired", now, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("refreshed session remained eligible for expiration after revalidation")
	}
}

func TestSessionAdminGCSkipsLockedSessionTempsAndPurgesLegacyStagingAfterUnlock(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	location, _ := createAdminTestSession(t, store, "active.bin", now)
	oldTemp := filepath.Join(location.Dir, ".payload.json.orphan")
	stagingDir := filepath.Join(location.Dir, ".legacy-import.orphan")
	if err := os.WriteFile(oldTemp, []byte("tmp"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-25 * time.Hour)
	if err := os.Chtimes(oldTemp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stagingDir, old, old); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GC(SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour}); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	for _, path := range []string{oldTemp, stagingDir} {
		if _, err := os.Stat(path); err != nil {
			_ = lock.Close()
			t.Fatalf("locked session temp was removed: %s: %v", path, err)
		}
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GC(SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldTemp, stagingDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unlocked old session temp survived GC: %s: %v", path, err)
		}
	}
}

func TestSessionAdminGCPurgesOldDownloadTempOnly(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	location, _ := createAdminTestSession(t, store, "download-temp.bin", now)
	partsDir := location.PartsDir
	if err := os.MkdirAll(partsDir, 0700); err != nil {
		t.Fatal(err)
	}
	oldTemp := filepath.Join(partsDir, ".abc.download.json.old")
	youngTemp := filepath.Join(partsDir, ".abc.download.json.young")
	for _, path := range []string{oldTemp, youngTemp} {
		if err := os.WriteFile(path, []byte("tmp"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldTemp, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(youngTemp, now.Add(-23*time.Hour), now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	actions, err := store.GC(SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldTemp); !os.IsNotExist(err) {
		t.Fatalf("old download temp was not purged: %v actions=%#v", err, actions)
	}
	if _, err := os.Stat(youngTemp); err != nil {
		t.Fatalf("young download temp was removed: %v", err)
	}
}

func TestSessionAdminGCExpiresOldAndPreservesNewerVersion(t *testing.T) {
	now := time.Now().UTC()
	store := SessionStore{Root: t.TempDir()}
	oldLocation, _ := createAdminTestSession(t, store, "old.bin", now.Add(-40*24*time.Hour))
	newerLocation, newerManifest := createAdminTestSession(t, store, "newer.bin", now.Add(-90*24*time.Hour))
	newerManifest.Version = SessionManifestVersion + 1
	data, err := json.Marshal(newerManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newerLocation.ManifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	actions, err := store.GC(SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawTrash, sawNewerSkip bool
	for _, action := range actions {
		if action.Path == oldLocation.Dir && action.Action == "trash" {
			sawTrash = true
		}
		if action.Path == newerLocation.Dir && action.Action == "skip" && action.Reason == "newer-version" {
			sawNewerSkip = true
		}
	}
	if !sawTrash || !sawNewerSkip {
		t.Fatalf("unexpected dry-run actions: %#v", actions)
	}
	if _, err := os.Stat(oldLocation.Dir); err != nil {
		t.Fatalf("dry-run mutated old session: %v", err)
	}
	actions, err = store.GC(SessionGCOptions{Now: now, Retention: 30 * 24 * time.Hour, TrashRetention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldLocation.Dir); !os.IsNotExist(err) {
		t.Fatalf("expired session was not trashed: %v actions=%#v", err, actions)
	}
	if _, err := os.Stat(newerLocation.Dir); err != nil {
		t.Fatalf("newer-version session was removed: %v", err)
	}
}
