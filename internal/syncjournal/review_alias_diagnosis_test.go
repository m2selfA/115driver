package syncjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestDiagnoseReviewAliasesSharesCurrentTrashAndVerifierClassification(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	livePlan := currentTestPlan()
	liveHandle, err := store.CreateCurrent(livePlan)
	if err != nil {
		t.Fatal(err)
	}
	liveReview := "sha256:" + strings.Repeat("1", 64)
	if _, err := liveHandle.BindReviewAlias(liveReview); err != nil {
		t.Fatal(err)
	}
	if err := liveHandle.Close(); err != nil {
		t.Fatal(err)
	}

	mismatchReview := "sha256:" + strings.Repeat("2", 64)
	if _, err := store.WriteReviewAlias(mismatchReview, livePlan.PlanID); err != nil {
		t.Fatal(err)
	}
	orphanReview := "sha256:" + strings.Repeat("3", 64)
	orphanPlan := strings.Repeat("4", 64)
	if _, err := store.WriteReviewAlias(orphanReview, orphanPlan); err != nil {
		t.Fatal(err)
	}

	softPlan := currentTestPlan()
	softPlan.RemoteRootID = "soft"
	softPlan.PlanID = ""
	softPlan.PlanID = syncplanpkg.Fingerprint(softPlan)
	softHandle, err := store.CreateCurrent(softPlan)
	if err != nil {
		t.Fatal(err)
	}
	softReview := "sha256:" + strings.Repeat("5", 64)
	if _, err := softHandle.BindReviewAlias(softReview); err != nil {
		t.Fatal(err)
	}
	if err := softHandle.Close(); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(softPlan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveDirectoryToSessionTrash(store.Root, location.Dir, softPlan.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	scan, err := store.DiagnoseReviewAliases(16, 16, func(alias ReviewAlias, journal Journal) (bool, error) {
		return alias.ReviewID != mismatchReview, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Scanned != 4 || scan.Live != 1 || scan.Orphan != 1 || scan.SoftDeleted != 1 || scan.IdentityMismatch != 1 || scan.Invalid != 0 || scan.Issues != 3 {
		t.Fatalf("diagnosis aggregate=%#v", scan)
	}
	statuses := map[string]ReviewAliasDiagnosisStatus{}
	for _, entry := range scan.Entries {
		statuses[entry.Alias.ReviewID] = entry.Status
	}
	if statuses[liveReview] != ReviewAliasDiagnosisLive || statuses[mismatchReview] != ReviewAliasDiagnosisIdentityMismatch || statuses[orphanReview] != ReviewAliasDiagnosisOrphan || statuses[softReview] != ReviewAliasDiagnosisSoftDeleted {
		t.Fatalf("diagnosis statuses=%#v", statuses)
	}
}

func TestDiagnoseReviewAliasesFailsClosedWhenTrashEvidenceIsInvalid(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("c", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("7", 64)
	planID := strings.Repeat("8", 64)
	if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
		t.Fatal(err)
	}
	trashRoot, err := store.trashRoot()
	if err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(trashRoot, "sync-journal-broken-"+planID)
	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trashDir, "journal.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if scan, err := store.DiagnoseReviewAliases(8, 8, nil); !errors.Is(err, ErrInvalidSchema) || scan.Scanned != 0 || scan.Orphan != 0 {
		t.Fatalf("invalid trash diagnosis=%#v err=%v", scan, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("fail-closed diagnosis mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestDiagnoseReviewAliasesIgnoresUnrelatedInvalidTrash(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("d", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("9", 64)
	planID := strings.Repeat("a", 64)
	if _, err := store.WriteReviewAlias(reviewID, planID); err != nil {
		t.Fatal(err)
	}
	trashRoot, err := store.trashRoot()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedPlanID := strings.Repeat("b", 64)
	trashDir := filepath.Join(trashRoot, "sync-journal-broken-"+unrelatedPlanID)
	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trashDir, "journal.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}

	scan, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || scan.Scanned != 1 || scan.Orphan != 1 || scan.Issues != 1 || len(scan.Entries) != 1 || scan.Entries[0].Status != ReviewAliasDiagnosisOrphan {
		t.Fatalf("unrelated invalid trash blocked orphan proof: scan=%#v err=%v", scan, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("targeted trash diagnosis mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestDiagnoseReviewAliasesProfileUsesPersistedAliasAccount(t *testing.T) {
	root := t.TempDir()
	profile := strings.Repeat("b", 64)
	store := Store{Root: root, ProfileScope: profile, AccountID: 42}
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("6", 64)
	if _, err := handle.BindReviewAlias(reviewID); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	offline := store
	offline.AccountID = 0
	scan, err := offline.DiagnoseReviewAliasesProfile(8, 8, nil)
	if err != nil || scan.Live != 1 || len(scan.Entries) != 1 || scan.Entries[0].Alias.AccountID != 42 {
		t.Fatalf("offline alias diagnosis=%#v err=%v", scan, err)
	}
	if _, err := offline.DiagnoseReviewAliases(8, 8, nil); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("strict unknown-account diagnosis error=%v", err)
	}
}
