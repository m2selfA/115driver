package syncjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trashMaintenanceFixture(t *testing.T, store Store, reviewID string) (TrashedCurrentRecord, string) {
	t.Helper()
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	trashPath, err := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC())
	closeErr := guard.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("trash fixture path=%q err=%v close=%v", trashPath, err, closeErr)
	}
	scan, err := store.ScanTrashedCurrent(16)
	if err != nil || len(scan.Records) != 1 || scan.Invalid != 0 || scan.MigrationRequired != 0 {
		t.Fatalf("trash scan=%#v err=%v", scan, err)
	}
	return scan.Records[0], trashPath
}

func TestScanAndRestoreTrashedCurrentReviewedRoundTrip(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42, Retention: time.Hour}
	reviewID := "sha256:" + strings.Repeat("b", 64)
	record, trashPath := trashMaintenanceFixture(t, store, reviewID)
	if record.Journal.State != StatusCompleted || record.Journal.PlanID == "" || record.TrashName != filepath.Base(trashPath) || record.TrashedAt.IsZero() {
		t.Fatalf("unexpected trashed record: %#v", record)
	}
	if _, err := store.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trash fixture alias survived: %v", err)
	}

	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreTrashedCurrentReviewed(
		guard, reviewID, record.TrashName, record.Journal.PlanID,
		record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs,
	)
	closeErr := guard.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("restore trashed journal err=%v close=%v", err, closeErr)
	}
	if restored.PlanID != record.Journal.PlanID || restored.State != record.Journal.State {
		t.Fatalf("restored journal changed identity/state: %#v", restored)
	}
	if _, err := os.Stat(trashPath); !os.IsNotExist(err) {
		t.Fatalf("restored trash directory still exists: %v", err)
	}
	if current, err := store.InspectCurrent(record.Journal.PlanID); err != nil || current.PlanID != record.Journal.PlanID {
		t.Fatalf("restored current journal missing: current=%#v err=%v", current, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != record.Journal.PlanID {
		t.Fatalf("restored review alias mismatch: resolved=%q err=%v", resolved, err)
	}
}

func TestRestoreTrashedCurrentReviewedFailsClosedWhenCurrentReappears(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("c", 64), AccountID: 42, Retention: time.Hour}
	reviewID := "sha256:" + strings.Repeat("d", 64)
	record, trashPath := trashMaintenanceFixture(t, store, reviewID)
	handle, err := store.CreateCurrent(record.Journal.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	_, restoreErr := store.RestoreTrashedCurrentReviewed(
		guard, reviewID, record.TrashName, record.Journal.PlanID,
		record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs,
	)
	_ = guard.Close()
	if !errors.Is(restoreErr, ErrRestoreCurrentExists) {
		t.Fatalf("current collision restore error=%v", restoreErr)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("current collision consumed trash entry: %v", err)
	}
}

func TestRestoreTrashedCurrentFailsClosedOnMalformedAliasSidecar(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("6", 64), AccountID: 42, Retention: time.Hour}
	reviewID := "sha256:" + strings.Repeat("3", 64)
	record, trashPath := trashMaintenanceFixture(t, store, reviewID)
	if err := os.WriteFile(filepath.Join(trashPath, trashReviewAliasesFile), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	_, restoreErr := store.RestoreTrashedCurrent(guard, record.TrashName, record.Journal.PlanID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	_ = guard.Close()
	if !errors.Is(restoreErr, ErrInvalidSchema) {
		t.Fatalf("malformed sidecar restore error=%v", restoreErr)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("malformed sidecar restore consumed trash: %v", err)
	}
	if _, err := store.InspectCurrent(record.Journal.PlanID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed sidecar restore created current journal: %v", err)
	}
}

func TestRestoreTrashedCurrentFailsClosedOnReviewAliasConflict(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("5", 64), AccountID: 42, Retention: time.Hour}
	reviewID := "sha256:" + strings.Repeat("4", 64)
	record, trashPath := trashMaintenanceFixture(t, store, reviewID)
	conflictingPlanID := strings.Repeat("9", 64)
	if conflictingPlanID == record.Journal.PlanID {
		conflictingPlanID = strings.Repeat("8", 64)
	}
	if _, err := store.WriteReviewAlias(reviewID, conflictingPlanID); err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	_, restoreErr := store.RestoreTrashedCurrent(guard, record.TrashName, record.Journal.PlanID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	_ = guard.Close()
	if !errors.Is(restoreErr, ErrReviewAliasConflict) {
		t.Fatalf("alias-conflict restore error=%v", restoreErr)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("alias-conflict restore consumed trash: %v", err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != conflictingPlanID {
		t.Fatalf("alias-conflict restore mutated conflicting alias: resolved=%q err=%v", resolved, err)
	}
}

func TestScanTrashedCurrentOmitsForeignAccountAndBoundsCandidates(t *testing.T) {
	root := t.TempDir()
	own := Store{Root: root, ProfileScope: strings.Repeat("e", 64), AccountID: 42, Retention: time.Hour}
	foreign := own
	foreign.AccountID = 99
	ownReview := "sha256:" + strings.Repeat("1", 64)
	foreignReview := "sha256:" + strings.Repeat("2", 64)
	_, _ = trashMaintenanceFixture(t, own, ownReview)
	_, _ = trashMaintenanceFixture(t, foreign, foreignReview)

	scan, err := own.ScanTrashedCurrent(16)
	if err != nil || len(scan.Records) != 1 || scan.Records[0].Journal.AccountID != 42 {
		t.Fatalf("account-bound trash scan=%#v err=%v", scan, err)
	}
	if _, err := own.ScanTrashedCurrent(1); err != nil {
		// Foreign entries count toward the hard filesystem scan bound even though
		// their metadata is not exposed, so exceeding the bound must fail closed.
		if !errors.Is(err, ErrTrashScanLimit) {
			t.Fatalf("bounded trash scan error=%v", err)
		}
	} else {
		t.Fatal("bounded trash scan unexpectedly ignored a second sync-journal directory")
	}
}
