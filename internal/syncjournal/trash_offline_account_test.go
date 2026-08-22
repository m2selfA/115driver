package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOfflineUnknownAccountTrashRoundTripBorrowsPersistedJournalBinding(t *testing.T) {
	owner := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("d", 64), AccountID: 42}
	plan := currentTestPlan()
	handle, err := owner.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewA := "sha256:" + strings.Repeat("1", 64)
	reviewB := "sha256:" + strings.Repeat("2", 64)
	for _, reviewID := range []string{reviewA, reviewB} {
		if _, err := handle.BindReviewAlias(reviewID); err != nil {
			handle.Close()
			t.Fatal(err)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	offline := owner
	offline.AccountID = 0
	trashDir, err := offline.TrashCurrent(plan.PlanID, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("offline trash with persisted account binding failed: %v", err)
	}
	aliases, found, err := ReadTrashReviewAliases(trashDir)
	if err != nil || !found || len(aliases) != 2 {
		t.Fatalf("offline trash aliases=%#v found=%v err=%v", aliases, found, err)
	}
	for _, reviewID := range []string{reviewA, reviewB} {
		if _, err := owner.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("offline trash left review alias %q: %v", reviewID, err)
		}
	}

	scan, err := offline.ScanTrashedCurrent(16)
	if err != nil || len(scan.Records) != 1 {
		t.Fatalf("offline trash scan=%#v err=%v", scan, err)
	}
	record := scan.Records[0]
	guard, err := offline.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := offline.RestoreTrashedCurrent(guard, record.TrashName, record.Journal.PlanID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	closeErr := guard.Close()
	if err != nil || closeErr != nil || restored.AccountID != owner.AccountID {
		t.Fatalf("offline restore journal=%#v err=%v close=%v", restored, err, closeErr)
	}
	for _, reviewID := range []string{reviewA, reviewB} {
		resolved, err := owner.ResolveReviewAlias(reviewID)
		if err != nil || resolved != plan.PlanID {
			t.Fatalf("offline-restored alias %q resolved=%q err=%v", reviewID, resolved, err)
		}
	}
}
