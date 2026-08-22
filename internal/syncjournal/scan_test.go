package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func scanTestPlan(relative string) syncplanpkg.Plan {
	plan := currentTestPlan()
	plan.Items[0].RelativePath = relative
	plan.Items[0].LocalPath = "/local/" + relative
	plan.PlanID = ""
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	return plan
}

func TestScanCurrentSortsDetectsLocksAndCountsLegacy(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	firstPlan := scanTestPlan("first.bin")
	secondPlan := scanTestPlan("second.bin")

	first, err := store.CreateCurrent(firstPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Mutate(func(journal *Journal) error {
		journal.UpdatedAt = time.Unix(100, 0).UTC()
		return nil
	}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.CreateCurrent(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Mutate(func(journal *Journal) error {
		journal.UpdatedAt = time.Unix(200, 0).UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	legacyLocation, err := store.Location(strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.WritePrivateFileAtomic(legacyLocation.JournalPath, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}

	scan, err := store.ScanCurrent(10)
	if err != nil {
		t.Fatal(err)
	}
	if scan.MigrationRequired != 1 || len(scan.Records) != 2 {
		t.Fatalf("scan result = %#v", scan)
	}
	if scan.Records[0].Journal.PlanID != secondPlan.PlanID || !scan.Records[0].InUse {
		t.Fatalf("newest/in-use scan record = %#v", scan.Records[0])
	}
	if scan.Records[1].Journal.PlanID != firstPlan.PlanID || scan.Records[1].InUse {
		t.Fatalf("older/unlocked scan record = %#v", scan.Records[1])
	}
}

func TestScanCurrentFailsClosedAtLimit(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	for _, relative := range []string{"a.bin", "b.bin"} {
		handle, err := store.CreateCurrent(scanTestPlan(relative))
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ScanCurrent(1); !errors.Is(err, ErrScanLimit) {
		t.Fatalf("bounded scan error = %v, want ErrScanLimit", err)
	}
}
