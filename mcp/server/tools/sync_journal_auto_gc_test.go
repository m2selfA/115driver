package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
)

func createOldMCPSharedTrashDir(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, "trash", name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "journal.json"), []byte("trash"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPSessionOpportunisticGCPurgesSharedSyncJournalTrash(t *testing.T) {
	root := t.TempDir()
	trash := createOldMCPSharedTrashDir(t, root, "sync-journal-expired")
	store := syncjournalpkg.Store{
		Root: root, ProfileScope: strings.Repeat("a", 64), AccountID: 42,
		AutoGC: true, GCInterval: time.Nanosecond,
		Retention: 24 * time.Hour, TrashRetention: 24 * time.Hour,
	}
	ft := NewFileTools(nil, WithSyncJournalStore(&store))
	if err := ft.runMCPSessionOpportunisticGC(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("expired shared sync journal trash was not purged: %v", err)
	}
}

func TestMCPSessionOpportunisticGCDisabledLeavesSharedTrash(t *testing.T) {
	root := t.TempDir()
	trash := createOldMCPSharedTrashDir(t, root, "sync-journal-expired")
	store := syncjournalpkg.Store{
		Root: root, ProfileScope: strings.Repeat("a", 64), AccountID: 42,
		AutoGC: false, GCInterval: time.Nanosecond,
		Retention: 24 * time.Hour, TrashRetention: 24 * time.Hour,
	}
	ft := NewFileTools(nil, WithSyncJournalStore(&store))
	if err := ft.runMCPSessionOpportunisticGC(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("disabled auto-GC touched shared trash: %v", err)
	}
}

func TestExecuteSyncPlanRunsConfiguredSessionOpportunisticGC(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	trash := createOldMCPSharedTrashDir(t, fixture.store.Root, "sync-journal-expired-before-sync")
	fixture.ft.syncJournalStore.AutoGC = true
	fixture.ft.syncJournalStore.GCInterval = time.Nanosecond
	fixture.ft.syncJournalStore.Retention = 24 * time.Hour
	fixture.ft.syncJournalStore.TrashRetention = 24 * time.Hour

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" {
		t.Fatalf("sync execution with auto-GC result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("sync execution did not opportunistically purge shared trash: %v", err)
	}
}
