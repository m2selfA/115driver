package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionIdentityV2Stable160BitID(t *testing.T) {
	if _, err := NewSessionIdentityV2("upload", "file", "scope", filepath.Join(t.TempDir(), "file.bin"), "/bad\x00remote", "multipart", "single-file"); err == nil {
		t.Fatal("identity accepted embedded NUL")
	}
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "source"), "/Remote/Case/../Tree", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	id, full, err := identity.SessionID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 || len(full) != 64 {
		t.Fatalf("unexpected id/hash lengths: %q %q", id, full)
	}
	if id != strings.ToLower(id) {
		t.Fatalf("session id must be lowercase base32: %q", id)
	}
	if identity.RemotePath != "/Remote/Tree" {
		t.Fatalf("remote path case/cleaning changed unexpectedly: %q", identity.RemotePath)
	}
}

func TestSessionStoreLayoutAndAccountValidation(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("download", "tree", scope, filepath.Join(t.TempDir(), "dst"), "/A/B", "chunk", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "dst", 42)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(location.ManifestPath) != "session.json" || filepath.Base(location.PayloadPath) != "payload.json" || filepath.Base(location.PartsDir) != "parts" {
		t.Fatalf("unexpected managed layout: %#v", location)
	}
	if manifest.AccountID != 42 || manifest.State != "active" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	if _, _, err := store.Open(identity, "dst", 99); !errors.Is(err, ErrSessionAccountMismatch) {
		t.Fatalf("expected account mismatch, got %v", err)
	}
}

func TestSessionStoreFindsExistingSessionWhenSlugChanges(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "source"), "/Remote", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, _, err := store.Open(identity, "old-name", 42)
	if err != nil {
		t.Fatal(err)
	}
	legacySlugDir := filepath.Join(filepath.Dir(location.Dir), "legacy-slug--"+location.ID)
	if err := os.Rename(location.Dir, legacySlugDir); err != nil {
		t.Fatal(err)
	}
	found, err := store.Location(identity, "new-name")
	if err != nil {
		t.Fatal(err)
	}
	if found.Dir != legacySlugDir {
		t.Fatalf("slug change lost existing session: got %q want %q", found.Dir, legacySlugDir)
	}
}

func TestSessionStoreFindsValidSiblingWhenExpectedSlugIsCorrupt(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "source"), "/Remote", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, _, err := store.Open(identity, "old-name", 42)
	if err != nil {
		t.Fatal(err)
	}
	validSibling := filepath.Join(filepath.Dir(location.Dir), "legacy-slug--"+location.ID)
	if err := os.Rename(location.Dir, validSibling); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(filepath.Dir(validSibling), SessionDisplaySlug("new-name")+"--"+location.ID)
	if err := os.MkdirAll(expected, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(expected, "session.json"), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	found, err := store.Location(identity, "new-name")
	if err != nil {
		t.Fatal(err)
	}
	if found.Dir != validSibling {
		t.Fatalf("corrupt expected slug hid valid sibling: got %q want %q", found.Dir, validSibling)
	}
}

func TestImportLegacySessionCopiesPayloadAndParts(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "source"), "/Remote", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, _, err := store.Open(identity, "source", 42)
	if err != nil {
		t.Fatal(err)
	}
	legacyDir := t.TempDir()
	legacyPayload := filepath.Join(legacyDir, "legacy.session.json")
	legacyParts := legacyPayload + ".parts"
	if err := os.WriteFile(legacyPayload, []byte(`{"legacy":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyParts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyParts, "part.json"), []byte("part"), 0600); err != nil {
		t.Fatal(err)
	}
	imported, err := ImportLegacySession(location, legacyPayload, legacyParts)
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		t.Fatal("legacy session was not imported")
	}
	if data, err := os.ReadFile(location.PayloadPath); err != nil || string(data) != `{"legacy":true}` {
		t.Fatalf("unexpected imported payload: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(location.PartsDir, "part.json")); err != nil || string(data) != "part" {
		t.Fatalf("unexpected imported part: %q err=%v", data, err)
	}
	managed, err := RemoveManagedSessionForPayload(location.PayloadPath)
	if err != nil || !managed {
		t.Fatalf("managed cleanup failed: managed=%v err=%v", managed, err)
	}
	if _, err := os.Stat(location.Dir); !os.IsNotExist(err) {
		t.Fatalf("managed session directory survived cleanup: %v", err)
	}
}

func TestImportLegacySessionDoesNotCommitPayloadBeforeParts(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "source"), "/Remote", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, _, err := store.Open(identity, "source", 42)
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload := filepath.Join(t.TempDir(), "legacy.session.json")
	legacyParts := legacyPayload + ".parts"
	if err := os.WriteFile(legacyPayload, []byte(`{"legacy":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyParts, []byte("not-a-directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacySession(location, legacyPayload, legacyParts); err == nil {
		t.Fatal("invalid legacy parts unexpectedly imported")
	}
	if _, err := os.Stat(location.PayloadPath); !os.IsNotExist(err) {
		t.Fatalf("payload was committed before parts validation completed: %v", err)
	}
}

func TestTouchManagedSessionForStatePathUpdatesPartsManifest(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("download", "tree", scope, filepath.Join(t.TempDir(), "dst"), "/Remote", "file", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "dst", 42)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	manifest.UpdatedAt = old
	manifest.LastUsedAt = old
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(location.PartsDir, "file.download.json")
	if err := os.MkdirAll(location.PartsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if touched, err := TouchManagedSessionForStatePath(statePath, true); err != nil || !touched {
		t.Fatalf("managed parts touch failed: touched=%v err=%v", touched, err)
	}
	data, err := os.ReadFile(location.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var after SessionManifest
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.After(old) || !after.LastUsedAt.After(old) {
		t.Fatalf("managed state touch did not refresh timestamps: %#v", after)
	}
}

func TestTouchManagedSessionForStatePathSerializesConcurrentWriters(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "tree", scope, filepath.Join(t.TempDir(), "src"), "/Remote", "multipart", "directory")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, _, err := store.Open(identity, "src", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(location.PartsDir, 0700); err != nil {
		t.Fatal(err)
	}

	const workers = 12
	const iterations = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		statePath := filepath.Join(location.PartsDir, fmt.Sprintf("file-%02d.upload.json", worker))
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				touched, touchErr := TouchManagedSessionForStatePath(path, true)
				if touchErr != nil {
					errs <- touchErr
					return
				}
				if !touched {
					errs <- fmt.Errorf("managed session unexpectedly disappeared")
					return
				}
			}
		}(statePath)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent managed session touch failed: %v", err)
	}

	data, err := os.ReadFile(location.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest SessionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("concurrent manifest writes left invalid JSON: %v", err)
	}
	if err := validateSessionManifest(manifest); err != nil {
		t.Fatalf("concurrent manifest writes left invalid state: %v", err)
	}
	if manifest.State != "active" || manifest.SessionID != location.ID {
		t.Fatalf("concurrent manifest writes changed identity/state: %#v", manifest)
	}
}

func TestSessionStoreOpenRefreshesLastUsedWithoutMutatingUpdatedAt(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("download", "file", scope, filepath.Join(t.TempDir(), "dst.bin"), "/Remote/file", "file", "single-file")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "dst.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	manifest.UpdatedAt = old
	manifest.LastUsedAt = old
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	_, reopened, err := store.Open(identity, "dst.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.UpdatedAt.Equal(old) || !reopened.LastUsedAt.After(old) {
		t.Fatalf("open changed mutation time or failed to refresh use time: %#v", reopened)
	}
}

func TestSessionStoreOpenDoesNotReactivateCompletedSession(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "file", scope, filepath.Join(t.TempDir(), "file.bin"), "/Remote", "multipart", "single-file")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "file.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	manifest.State = "completed"
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(identity, "file.bin", 42); !errors.Is(err, ErrSessionCompleted) {
		t.Fatalf("completed residual was not detected: %v", err)
	}
	data, err := os.ReadFile(location.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var after SessionManifest
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.State != "completed" {
		t.Fatalf("completed residual was reactivated: %#v", after)
	}
}

func TestQuarantineCorruptLocationRebuildsWithoutTouchingFutureVersion(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "file", scope, filepath.Join(t.TempDir(), "file.bin"), "/Remote", "multipart", "single-file")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "file.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := os.WriteFile(location.ManifestPath, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	trashPath, err := store.QuarantineCorruptLocation(location)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(location.Dir); !os.IsNotExist(err) {
		t.Fatalf("corrupt session survived quarantine: %v", err)
	}
	if info, err := os.Stat(trashPath); err != nil || !info.IsDir() {
		t.Fatalf("corrupt quarantine target missing: info=%v err=%v", info, err)
	}
	freshLocation, freshManifest, err := store.Open(identity, "file.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	if freshLocation.Dir == trashPath || freshManifest.State != "active" {
		t.Fatalf("fresh session was not rebuilt after quarantine: location=%#v manifest=%#v", freshLocation, freshManifest)
	}

	futureLocation, futureManifest, err := store.Open(identity, "future-name", 42)
	if err != nil {
		t.Fatal(err)
	}
	futureManifest.Version = SessionManifestVersion + 1
	encoded, err := json.Marshal(futureManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(futureLocation.ManifestPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QuarantineCorruptLocation(futureLocation); !errors.Is(err, ErrSessionNewerVersion) {
		t.Fatalf("future-version session was not protected from quarantine: %v", err)
	}
	if _, err := os.Stat(futureLocation.Dir); err != nil {
		t.Fatalf("future-version session was moved: %v", err)
	}
	_ = manifest
}

func TestSessionStorePreservesNewerManifest(t *testing.T) {
	scope, err := SessionProfileScope(filepath.Join(t.TempDir(), "config.toml"), "main")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSessionIdentityV2("upload", "file", scope, filepath.Join(t.TempDir(), "file.bin"), "/Remote", "multipart", "single-file")
	if err != nil {
		t.Fatal(err)
	}
	store := SessionStore{Root: t.TempDir()}
	location, manifest, err := store.Open(identity, "file.bin", 42)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Version = SessionManifestVersion + 1
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(location.ManifestPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(identity, "file.bin", 42); !errors.Is(err, ErrSessionNewerVersion) {
		t.Fatalf("expected newer-version preservation error, got %v", err)
	}
	data, err := os.ReadFile(location.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var after SessionManifest
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Version != SessionManifestVersion+1 {
		t.Fatalf("newer manifest was overwritten: %#v", after)
	}
}
