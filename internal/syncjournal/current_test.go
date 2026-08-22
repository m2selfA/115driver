package syncjournal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func currentTestPlan() syncplanpkg.Plan {
	plan := syncplanpkg.Plan{
		Operation: "sync", DryRun: true, Mode: syncplanpkg.ModeConservative,
		Direction: syncplanpkg.DirectionUpload, ConflictPolicy: syncplanpkg.ConflictError,
		Ready: true, LocalRoot: "/local", RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncplanpkg.Item{{RelativePath: "file.bin", Action: "upload", Kind: "file", LocalPresent: true, LocalPath: "/local/file.bin", LocalSize: 3, LocalSHA1: strings.Repeat("A", 40)}},
	}
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	return plan
}

func TestEncodeDecodeCurrentRoundTripAndStrictEnvelope(t *testing.T) {
	journal, err := New(currentTestPlan(), strings.Repeat("a", 64), 42, time.Unix(123, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	encoded, normalized, err := EncodeCurrent(journal)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCurrent(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PlanID != normalized.PlanID || decoded.Status != StatusActive || decoded.AccountID != 42 || len(decoded.Items) != 1 {
		t.Fatalf("current journal round-trip changed state: %#v", decoded)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "run_stats")
	missing, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCurrent(missing); !errors.Is(err, ErrInvalidSchema) || !strings.Contains(err.Error(), "run_stats") {
		t.Fatalf("current decoder accepted missing run_stats: %v", err)
	}
}

func TestDecodeCurrentRejectsLegacyAndFutureWithoutMigration(t *testing.T) {
	for name, data := range map[string][]byte{
		"legacy": []byte(`{"version":1}`),
		"future": []byte(`{"version":999}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeCurrent(data)
			if name == "legacy" && !errors.Is(err, ErrMigrationRequired) {
				t.Fatalf("legacy current decode error = %v", err)
			}
			if name == "future" && !errors.Is(err, ErrNewerVersion) {
				t.Fatalf("future current decode error = %v", err)
			}
		})
	}
}

func TestValidateMigrationHistoryCurrentContract(t *testing.T) {
	journal, err := New(currentTestPlan(), strings.Repeat("a", 64), 42, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.Migrations = []MigrationRecord{{FromVersion: 1, ToVersion: 2, MigratedAt: time.Now().UTC(), SourceSHA256: strings.Repeat("a", 64)}}
	if err := ValidateMigrationHistory(journal); err != nil {
		t.Fatalf("valid migration history rejected: %v", err)
	}
	journal.Migrations[0].SourceSHA256 = "bad"
	if err := ValidateMigrationHistory(journal); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("invalid migration history error = %v", err)
	}
}
