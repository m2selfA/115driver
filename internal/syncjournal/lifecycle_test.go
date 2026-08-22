package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestNewBuildsCanonicalInitialJournal(t *testing.T) {
	plan := syncplanpkg.Plan{
		Mode: syncplanpkg.ModeConservative, Direction: syncplanpkg.DirectionUpload, ConflictPolicy: syncplanpkg.ConflictError,
		LocalRoot: "/local", RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncplanpkg.Item{
			{RelativePath: "file.bin", Action: "upload", Kind: "file", LocalModTimeUnixNano: 11},
			{RelativePath: "same.bin", Action: "skip", Kind: "file", LocalModTimeUnixNano: 22, RemoteModTimeUnixNano: 33},
		},
	}
	plan.PlanID = strings.ToUpper(syncplanpkg.Fingerprint(plan))
	now := time.Unix(123, 456).UTC()
	journal, err := New(plan, "scope", 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != Version || journal.Schema != SchemaID || journal.PlanID != strings.ToLower(plan.PlanID) || journal.Status != StatusActive || !journal.CreatedAt.Equal(now) || !journal.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected initial journal header: %#v", journal)
	}
	if len(journal.Items) != 2 || journal.Items[0].State != "pending" || journal.Items[0].Phase != "pending" || journal.Items[1].State != "skipped" || journal.Items[1].Phase != "done" {
		t.Fatalf("unexpected initial journal items: %#v", journal.Items)
	}
	if journal.Items[0].LocalModTimeUnixNano != 11 || journal.Items[1].RemoteModTimeUnixNano != 33 {
		t.Fatalf("hidden snapshot metadata was not retained: %#v", journal.Items)
	}
}

func TestNewRejectsPlanIDFingerprintMismatch(t *testing.T) {
	plan := syncplanpkg.Plan{Mode: syncplanpkg.ModeConservative, Direction: syncplanpkg.DirectionUpload, ConflictPolicy: syncplanpkg.ConflictError, LocalRoot: "/local", RemoteRoot: "/remote", RemoteRootID: "root"}
	plan.PlanID = strings.Repeat("a", 64)
	if _, err := New(plan, "scope", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "fingerprint does not match") {
		t.Fatalf("mismatched plan ID was accepted: %v", err)
	}
}

func TestRestoreStoredItemRestoresHiddenSnapshotAndValidatesEnvelope(t *testing.T) {
	planned := syncplanpkg.Item{RelativePath: "file.bin", Action: "upload", Kind: "file"}
	stored := Item{Index: 0, RelativePath: "file.bin", Action: "upload", Kind: "file", State: "pending", Phase: "pending", LocalModTimeUnixNano: 12, RemoteModTimeUnixNano: 34}
	if err := RestoreStoredItem(0, stored, &planned); err != nil {
		t.Fatal(err)
	}
	if planned.LocalModTimeUnixNano != 12 || planned.RemoteModTimeUnixNano != 34 {
		t.Fatalf("stored hidden snapshots were not restored: %#v", planned)
	}

	badPhase := stored
	badPhase.Phase = "future-phase"
	if err := RestoreStoredItem(0, badPhase, &planned); !errors.Is(err, ErrInvalidSchema) || !strings.Contains(err.Error(), "invalid phase") {
		t.Fatalf("invalid phase error = %v", err)
	}
	badIdentity := stored
	badIdentity.RelativePath = "other.bin"
	if err := RestoreStoredItem(0, badIdentity, &planned); err == nil || !strings.Contains(err.Error(), "does not match stored plan") {
		t.Fatalf("identity mismatch was accepted: %v", err)
	}
}

func TestValidateJournalStateContract(t *testing.T) {
	for _, state := range []string{StatusActive, StatusFailed, StatusCompleted, StatusRecoveryRequired} {
		if err := ValidateJournalState(state); err != nil {
			t.Fatalf("valid journal state %q rejected: %v", state, err)
		}
	}
	if err := ValidateJournalState(StatusReconcileRequired); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("derived reconcile status was accepted as persisted state: %v", err)
	}
}
