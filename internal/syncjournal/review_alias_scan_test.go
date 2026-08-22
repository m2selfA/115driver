package syncjournal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReviewAliasesBuildsBoundedReverseIndex(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	planA := strings.Repeat("1", 64)
	planB := strings.Repeat("2", 64)
	reviews := []struct {
		reviewID string
		planID   string
	}{
		{"sha256:" + strings.Repeat("b", 64), planA},
		{"sha256:" + strings.Repeat("c", 64), planA},
		{"sha256:" + strings.Repeat("d", 64), planB},
	}
	for _, item := range reviews {
		if _, err := store.WriteReviewAlias(item.reviewID, item.planID); err != nil {
			t.Fatal(err)
		}
	}
	// Lock metadata is not an alias record and must not consume scan budget.
	lockPath, err := store.reviewAliasLockPath(reviews[0].reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	scan, err := store.ScanReviewAliases(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Aliases) != 3 || len(scan.ByPlanID) != 2 {
		t.Fatalf("review alias scan = %#v", scan)
	}
	if got := scan.ByPlanID[planA]; len(got) != 2 || got[0] != reviews[0].reviewID || got[1] != reviews[1].reviewID {
		t.Fatalf("reverse aliases for plan A = %#v", got)
	}
	if got := scan.ByPlanID[planB]; len(got) != 1 || got[0] != reviews[2].reviewID {
		t.Fatalf("reverse aliases for plan B = %#v", got)
	}
}

func TestScanReviewAliasesFailsClosedAtLimit(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	planID := strings.Repeat("1", 64)
	for _, hexChar := range []string{"b", "c"} {
		if _, err := store.WriteReviewAlias("sha256:"+strings.Repeat(hexChar, 64), planID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ScanReviewAliases(1); !errors.Is(err, ErrScanLimit) {
		t.Fatalf("review alias scan limit error = %v", err)
	}
}

func TestScanReviewAliasesRejectsMalformedOrNonCanonicalAlias(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("b", 64)
	alias, err := store.WriteReviewAlias(reviewID, strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.reviewAliasPath(reviewID)
	if err != nil {
		t.Fatal(err)
	}
	alias.AccountID = 99
	encoded, err := json.Marshal(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ScanReviewAliases(10); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("wrong-binding review alias scan error = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootPath()
	if err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(root, "review-aliases", "ff")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(badDir, strings.Repeat("c", 64)+".json")
	if err := os.WriteFile(badPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ScanReviewAliases(10); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("non-canonical review alias layout error = %v", err)
	}
}

func TestScanReviewAliasesEmptyStoreAndUnknownAccount(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	scan, err := store.ScanReviewAliases(10)
	if err != nil || len(scan.Aliases) != 0 || len(scan.ByPlanID) != 0 {
		t.Fatalf("empty review alias scan = %#v err=%v", scan, err)
	}
	if _, err := store.ScanReviewAliases(0); err == nil {
		t.Fatal("zero review alias scan limit was accepted")
	}

	reviewID := "sha256:" + strings.Repeat("d", 64)
	if _, err := store.WriteReviewAlias(reviewID, strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	unknown := store
	unknown.AccountID = 0
	if _, err := unknown.ScanReviewAliases(10); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("unknown-account review alias scan error = %v", err)
	}
	profileScan, err := unknown.ScanReviewAliasesProfile(10)
	if err != nil || len(profileScan.Aliases) != 1 || profileScan.Aliases[0].ReviewID != reviewID || profileScan.Aliases[0].AccountID != store.AccountID {
		t.Fatalf("profile-bound offline review alias scan=%#v err=%v", profileScan, err)
	}
}
