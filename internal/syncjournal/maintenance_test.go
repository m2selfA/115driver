package syncjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func completeCurrentForMaintenanceTest(t *testing.T, store Store, age time.Duration) (string, time.Time) {
	t.Helper()
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	aged := time.Now().UTC().Add(-age)
	if err := handle.Mutate(func(journal *Journal) error {
		journal.State = StatusCompleted
		journal.Status = StatusCompleted
		journal.Items[0].State = "succeeded"
		journal.Items[0].Phase = PhaseDone
		journal.Items[0].Post = &Postcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "remote-id", Size: 3, SHA1: strings.Repeat("B", 40)}
		journal.CompletedAt = &aged
		return nil
	}); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := store.InspectCurrent(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	journal.UpdatedAt = aged
	journal.CompletedAt = &aged
	location, err := store.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.WriteCurrent(location, journal)
	if err != nil {
		t.Fatal(err)
	}
	return plan.PlanID, written.UpdatedAt
}

func TestTrashCurrentReviewedRemovesAliasAndMovesJournalToSharedTrash(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42, Retention: 24 * time.Hour}
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewID := "sha256:" + strings.Repeat("c", 64)
	if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	now := time.Now().UTC()
	trashPath, err := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(trashPath); err != nil || !info.IsDir() {
		t.Fatalf("trash target missing: info=%v err=%v", info, err)
	} else if delta := info.ModTime().Sub(now); delta < -2*time.Second || delta > 2*time.Second {
		t.Fatalf("trash target did not receive move timestamp: got=%s want~=%s delta=%s", info.ModTime(), now, delta)
	}
	if _, err := store.InspectCurrent(planID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed current journal remains readable: %v", err)
	}
	if _, err := store.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("review alias survived trash: %v", err)
	}
	if filepath.Dir(trashPath) != filepath.Join(store.Root, "trash") {
		t.Fatalf("journal moved outside shared session trash: %s", trashPath)
	}
	// The common Session Store GC must measure trash retention from the move
	// time, not the much older journal activity time.
	sessionStore := transfer.SessionStore{Root: store.Root}
	if _, err := sessionStore.GC(transfer.SessionGCOptions{Now: now.Add(30 * time.Minute), Retention: 24 * time.Hour, TrashRetention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("freshly trashed journal was purged before trash retention: %v", err)
	}
	if _, err := sessionStore.GC(transfer.SessionGCOptions{Now: now.Add(2 * time.Hour), Retention: 24 * time.Hour, TrashRetention: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trashPath); !os.IsNotExist(err) {
		t.Fatalf("expired trashed journal was not purged after trash retention: %v", err)
	}
}

func TestTrashCurrentReviewedPersistsCompleteReviewAliasSetAndRawRestoreRecreatesIt(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("7", 64), AccountID: 42, Retention: time.Hour}
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewA := "sha256:" + strings.Repeat("1", 64)
	reviewB := "sha256:" + strings.Repeat("2", 64)
	for _, reviewID := range []string{reviewB, reviewA} {
		if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	trashPath, err := store.TrashCurrentReviewed(guard, reviewA, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC())
	closeErr := guard.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("multi-alias trash path=%q err=%v close=%v", trashPath, err, closeErr)
	}
	aliases, found, err := ReadTrashReviewAliases(trashPath)
	if err != nil || !found || len(aliases) != 2 || aliases[0] != reviewA || aliases[1] != reviewB {
		t.Fatalf("trash review alias sidecar aliases=%v found=%v err=%v", aliases, found, err)
	}
	for _, reviewID := range []string{reviewA, reviewB} {
		if _, err := store.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("trashed review alias %s survived: %v", reviewID, err)
		}
	}
	scan, err := store.ScanTrashedCurrent(8)
	if err != nil || len(scan.Records) != 1 || !equalReviewIDSets(scan.Records[0].ReviewIDs, aliases) {
		t.Fatalf("trashed record lost review aliases: scan=%#v err=%v", scan, err)
	}
	record := scan.Records[0]
	guard, err = store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreTrashedCurrent(guard, record.TrashName, planID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	closeErr = guard.Close()
	if err != nil || closeErr != nil || restored.PlanID != planID {
		t.Fatalf("raw multi-alias restore journal=%#v err=%v close=%v", restored, err, closeErr)
	}
	for _, reviewID := range []string{reviewA, reviewB} {
		resolved, err := store.ResolveReviewAlias(reviewID)
		if err != nil || resolved != planID {
			t.Fatalf("restored review alias %s resolved=%q err=%v", reviewID, resolved, err)
		}
	}
	location, err := store.Location(planID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(location.Dir, trashReviewAliasesFile)); !os.IsNotExist(err) {
		t.Fatalf("trash review alias sidecar leaked into restored current journal: %v", err)
	}
}

func TestTrashCurrentReviewedFailsClosedWhenCandidateChanged(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("b", 64), AccountID: 42, Retention: time.Hour}
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewID := "sha256:" + strings.Repeat("d", 64)
	if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(planID)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.ReadCurrent(location)
	if err != nil {
		t.Fatal(err)
	}
	journal.UpdatedAt = time.Now().UTC()
	if _, err := store.WriteCurrent(location, journal); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if _, err := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC()); !errors.Is(err, ErrCleanupCandidateChanged) {
		t.Fatalf("changed cleanup candidate error=%v", err)
	}
	if _, err := store.InspectCurrent(planID); err != nil {
		t.Fatalf("changed candidate was trashed: %v", err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("changed candidate alias mutated: resolved=%q err=%v", resolved, err)
	}
}

func TestAcquireCleanupGuardRefusesBulkMigrationMarker(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("e", 64), AccountID: 42}
	root, err := store.RootPath()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "migration", "migrate-all.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(`{"schema":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if guard != nil {
		_ = guard.Close()
	}
	if !errors.Is(err, ErrCleanupMigrationInProgress) {
		t.Fatalf("migration marker cleanup guard error=%v", err)
	}
}
