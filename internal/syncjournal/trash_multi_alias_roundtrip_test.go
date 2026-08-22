package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMultiAliasTrashRoundTripRestoresEveryReviewedBinding(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	firstReviewID := "sha256:" + strings.Repeat("b", 64)
	secondReviewID := "sha256:" + strings.Repeat("c", 64)
	if _, err := handle.BindReviewAlias(firstReviewID); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if _, err := handle.BindReviewAlias(secondReviewID); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	trashDir, err := store.TrashCurrent(plan.PlanID, false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	sidecarIDs, found, err := ReadTrashReviewAliases(trashDir)
	if err != nil || !found || len(sidecarIDs) != 2 || sidecarIDs[0] != firstReviewID || sidecarIDs[1] != secondReviewID {
		t.Fatalf("trash sidecar aliases=%#v found=%v err=%v", sidecarIDs, found, err)
	}
	for _, reviewID := range []string{firstReviewID, secondReviewID} {
		if _, err := store.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("review alias %q survived trash move: %v", reviewID, err)
		}
	}

	scan, err := store.ScanTrashedCurrent(16)
	if err != nil || len(scan.Records) != 1 {
		t.Fatalf("trash scan=%#v err=%v", scan, err)
	}
	record := scan.Records[0]
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := store.RestoreTrashedCurrent(guard, record.TrashName, record.Journal.PlanID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	closeErr := guard.Close()
	if err != nil || closeErr != nil || restored.PlanID != plan.PlanID {
		t.Fatalf("restored journal=%#v err=%v close=%v", restored, err, closeErr)
	}
	if _, err := store.InspectCurrent(plan.PlanID); err != nil {
		t.Fatalf("restored current journal is unreadable: %v", err)
	}
	for _, reviewID := range []string{firstReviewID, secondReviewID} {
		resolved, err := store.ResolveReviewAlias(reviewID)
		if err != nil || resolved != plan.PlanID {
			t.Fatalf("review alias %q restored as %q, err=%v", reviewID, resolved, err)
		}
	}
}
