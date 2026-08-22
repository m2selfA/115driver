package syncjournal

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func testCurrentStore(t *testing.T, accountID int64) Store {
	t.Helper()
	return Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: accountID}
}

func TestStoreLocationMatchesExistingSyncJournalLayout(t *testing.T) {
	store := testCurrentStore(t, 42)
	plan := currentTestPlan()
	location, err := store.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(absolute, "sync", LayoutVersion, store.ProfileScope, plan.PlanID[:2], plan.PlanID)
	wantLock := filepath.Join(absolute, "sync-locks", store.ProfileScope, plan.PlanID[:2], plan.PlanID+".lock")
	if location.Dir != wantDir || location.JournalPath != filepath.Join(wantDir, "journal.json") || location.LeasePath != filepath.Join(wantDir, "lease.json") || location.LockPath != wantLock {
		t.Fatalf("unexpected current journal location: %#v", location)
	}
}

func TestStoreCreateOpenMutateAndSharedLock(t *testing.T) {
	store := testCurrentStore(t, 42)
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, err := store.OpenCurrent(plan.PlanID); !errors.Is(err, transfer.ErrSessionLocked) {
		t.Fatalf("second current journal open did not contend on shared session lock: %v", err)
	}
	if err := handle.Mutate(func(journal *Journal) error {
		journal.Items[0].State = "running"
		journal.Items[0].Phase = "mutation-started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := handle.Snapshot(); got.Items[0].State != "running" || got.Items[0].Phase != "mutation-started" {
		t.Fatalf("mutated current journal snapshot = %#v", got.Items)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.OpenCurrent(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if got := opened.Snapshot(); got.Items[0].State != "running" || got.Items[0].Phase != "mutation-started" || got.Status != StatusActive {
		t.Fatalf("persisted current journal mutation = %#v", got)
	}
}

func TestScanCurrentIgnoresReviewAliasMetadata(t *testing.T) {
	store := testCurrentStore(t, 42)
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("b", 64)
	if _, err := store.WriteReviewAlias(reviewID, plan.PlanID); err != nil {
		t.Fatal(err)
	}

	scan, err := store.ScanCurrent(10)
	if err != nil {
		t.Fatal(err)
	}
	if scan.MigrationRequired != 0 || len(scan.Records) != 1 {
		t.Fatalf("scan with review alias = records=%d migration_required=%d", len(scan.Records), scan.MigrationRequired)
	}
	if got := scan.Records[0].Journal.PlanID; got != plan.PlanID {
		t.Fatalf("scan returned plan %q, want %q", got, plan.PlanID)
	}
	resolved, err := store.ResolveReviewAlias(reviewID)
	if err != nil || resolved != plan.PlanID {
		t.Fatalf("review alias after scan = %q, %v", resolved, err)
	}
}

func TestStoreCurrentBindingAndLegacyRefusal(t *testing.T) {
	root := t.TempDir()
	plan := currentTestPlan()
	store := Store{Root: root, ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	location := handle.Location()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	wrongAccount := Store{Root: root, ProfileScope: store.ProfileScope, AccountID: 7}
	if _, err := wrongAccount.InspectCurrent(plan.PlanID); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("current journal account mismatch error = %v", err)
	}

	if err := transfer.WritePrivateFileAtomic(location.JournalPath, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InspectCurrent(plan.PlanID); !errors.Is(err, ErrMigrationRequired) {
		t.Fatalf("legacy journal was not delegated to CLI migration: %v", err)
	}
	info, err := os.Stat(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("journal private-file mode = %v", info.Mode().Perm())
	}
}
