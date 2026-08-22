package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
)

func rewriteSyncJournalJSON(t *testing.T, location syncJournalLocation, mutate func(map[string]json.RawMessage)) []byte {
	t.Helper()
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	mutate(raw)
	updated, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(location.JournalPath, updated, 0600); err != nil {
		t.Fatal(err)
	}
	return updated
}

func findSyncJournalDoctorEntry(t *testing.T, report syncJournalDoctorReport, planID string) syncJournalDoctorEntry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.PlanID == planID {
			return entry
		}
	}
	t.Fatalf("doctor report omitted plan %s: %#v", planID, report.Entries)
	return syncJournalDoctorEntry{}
}

func findSyncJournalDoctorAliasEntry(t *testing.T, report syncJournalDoctorReport, reviewID string) syncJournalDoctorReviewAliasEntry {
	t.Helper()
	for _, entry := range report.ReviewAliases {
		if entry.ReviewID == reviewID {
			return entry
		}
	}
	t.Fatalf("doctor report omitted review alias %s: %#v", reviewID, report.ReviewAliases)
	return syncJournalDoctorReviewAliasEntry{}
}

func TestSyncJournalDoctorAuditsReviewAliasLifecycleWithoutMutation(t *testing.T) {
	store := testSyncJournalStore(t)
	shared := store.sharedCurrentStore()

	livePlan := testSyncJournalPlan(t)
	liveHandle, err := store.Create(livePlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveHandle.Close(); err != nil {
		t.Fatal(err)
	}
	liveReview := "sha256:" + strings.Repeat("1", 64)
	if _, err := shared.WriteReviewAlias(liveReview, livePlan.PlanID); err != nil {
		t.Fatal(err)
	}

	orphanReview := "sha256:" + strings.Repeat("2", 64)
	orphanPlanID := strings.Repeat("9", 64)
	if _, err := shared.WriteReviewAlias(orphanReview, orphanPlanID); err != nil {
		t.Fatal(err)
	}

	trashedPlan := testSyncJournalPlan(t)
	trashedHandle, err := store.Create(trashedPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := trashedHandle.Close(); err != nil {
		t.Fatal(err)
	}
	trashedReview := "sha256:" + strings.Repeat("3", 64)
	if _, err := shared.WriteReviewAlias(trashedReview, trashedPlan.PlanID); err != nil {
		t.Fatal(err)
	}
	trashedLocation, err := store.location(trashedPlan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncjournalpkg.MoveDirectoryToSessionTrash(store.Root, trashedLocation.Dir, trashedPlan.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewAliasesTotal != 3 || report.ReviewAliasesLive != 1 || report.ReviewAliasesOrphan != 1 || report.ReviewAliasesSoftDeleted != 1 || report.Issues != 2 || report.AllCurrentAndValid {
		t.Fatalf("review alias doctor aggregate=%#v", report)
	}
	if entry := findSyncJournalDoctorAliasEntry(t, report, liveReview); entry.Status != syncJournalDoctorAliasLive || entry.Error != "" {
		t.Fatalf("live alias diagnosis=%#v", entry)
	}
	if entry := findSyncJournalDoctorAliasEntry(t, report, orphanReview); entry.Status != syncJournalDoctorAliasOrphan || entry.SuggestedAction == "" {
		t.Fatalf("orphan alias diagnosis=%#v", entry)
	}
	if entry := findSyncJournalDoctorAliasEntry(t, report, trashedReview); entry.Status != syncJournalDoctorAliasSoftDeleted || entry.SuggestedAction == "" {
		t.Fatalf("soft-deleted alias diagnosis=%#v", entry)
	}
	for reviewID, planID := range map[string]string{liveReview: livePlan.PlanID, orphanReview: orphanPlanID, trashedReview: trashedPlan.PlanID} {
		resolved, err := shared.ResolveReviewAlias(reviewID)
		if err != nil || resolved != planID {
			t.Fatalf("read-only doctor mutated alias %q: resolved=%q err=%v", reviewID, resolved, err)
		}
	}
}

func TestSyncJournalDoctorReportsHealthyLegacyAndInUseWithoutMutation(t *testing.T) {
	store := testSyncJournalStore(t)

	healthyPlan := testSyncJournalPlan(t)
	healthy, err := store.Create(healthyPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}

	legacyPlan := testSyncJournalPlan(t)
	legacyLocation, legacyBefore := writeLegacySyncJournalV1(t, store, legacyPlan, nil)

	lockedPlan := testSyncJournalPlan(t)
	locked, err := store.Create(lockedPlan)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	if report.Total != 3 || report.Healthy != 2 || report.MigrationRequired != 1 || report.Issues != 1 || report.InUse != 1 || report.AllCurrentAndValid {
		t.Fatalf("doctor aggregate contract: %#v", report)
	}
	legacyEntry := findSyncJournalDoctorEntry(t, report, legacyPlan.PlanID)
	if legacyEntry.Health != syncJournalDoctorMigrationRequired || !legacyEntry.MigrationRequired || !strings.Contains(legacyEntry.SuggestedAction, "journal migrate") {
		t.Fatalf("legacy doctor entry: %#v", legacyEntry)
	}
	lockedEntry := findSyncJournalDoctorEntry(t, report, lockedPlan.PlanID)
	if lockedEntry.Health != syncJournalDoctorHealthy || !lockedEntry.InUse {
		t.Fatalf("in-use journal was misclassified: %#v", lockedEntry)
	}
	legacyAfter, err := os.ReadFile(legacyLocation.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatal("read-only doctor rewrote legacy journal")
	}
}

func TestSyncJournalDoctorClassifiesInvalidSchemaNewerVersionAndInvalidPath(t *testing.T) {
	store := testSyncJournalStore(t)

	invalidPlan := testSyncJournalPlan(t)
	invalidHandle, err := store.Create(invalidPlan)
	if err != nil {
		t.Fatal(err)
	}
	invalidLocation := invalidHandle.location
	if err := invalidHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, invalidLocation, func(raw map[string]json.RawMessage) {
		delete(raw, "run_stats")
	})

	futurePlan := testSyncJournalPlan(t)
	futureHandle, err := store.Create(futurePlan)
	if err != nil {
		t.Fatal(err)
	}
	futureLocation := futureHandle.location
	if err := futureHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, futureLocation, func(raw map[string]json.RawMessage) {
		raw["version"] = json.RawMessage("999")
	})

	root, err := store.root()
	if err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(root, "zz", "not-a-plan-id")
	if err := os.MkdirAll(invalidPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidPath, "journal.json"), []byte(`{"version":2}`), 0600); err != nil {
		t.Fatal(err)
	}

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	if report.Total != 3 || report.Issues != 3 || report.AllCurrentAndValid {
		t.Fatalf("doctor issue aggregate: %#v", report)
	}
	if entry := findSyncJournalDoctorEntry(t, report, invalidPlan.PlanID); entry.Health != syncJournalDoctorInvalidSchema || !strings.Contains(entry.Error, "run_stats") {
		t.Fatalf("invalid schema doctor classification: %#v", entry)
	}
	if entry := findSyncJournalDoctorEntry(t, report, futurePlan.PlanID); entry.Health != syncJournalDoctorNewerVersion {
		t.Fatalf("future schema doctor classification: %#v", entry)
	}
	foundInvalidPath := false
	for _, entry := range report.Entries {
		if entry.Health == syncJournalDoctorInvalidPath {
			foundInvalidPath = true
			break
		}
	}
	if !foundInvalidPath {
		t.Fatalf("doctor did not report invalid journal storage path: %#v", report.Entries)
	}
}

func TestSyncJournalReadRejectsStoragePlanIDMismatch(t *testing.T) {
	store := testSyncJournalStore(t)
	planA := testSyncJournalPlan(t)
	handleA, err := store.Create(planA)
	if err != nil {
		t.Fatal(err)
	}
	locationA := handleA.location
	if err := handleA.Close(); err != nil {
		t.Fatal(err)
	}
	planB := testSyncJournalPlan(t)
	handleB, err := store.Create(planB)
	if err != nil {
		t.Fatal(err)
	}
	locationB := handleB.location
	if err := handleB.Close(); err != nil {
		t.Fatal(err)
	}
	dataA, err := os.ReadFile(locationA.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locationB.JournalPath, dataA, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect(planB.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "storage path") {
		t.Fatalf("storage plan ID mismatch was accepted: %v", err)
	}
}

func TestSyncJournalRejectsInvalidTopLevelState(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	location := handle.location
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, location, func(raw map[string]json.RawMessage) {
		raw["state"] = json.RawMessage(`"mystery"`)
	})
	if _, err := store.Inspect(plan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "invalid journal state") {
		t.Fatalf("invalid top-level state was accepted: %v", err)
	}
}

func TestSyncJournalRejectsPersistedStatusMismatchAndUnknownPhase(t *testing.T) {
	store := testSyncJournalStore(t)
	statusPlan := testSyncJournalPlan(t)
	statusHandle, err := store.Create(statusPlan)
	if err != nil {
		t.Fatal(err)
	}
	statusLocation := statusHandle.location
	if err := statusHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, statusLocation, func(raw map[string]json.RawMessage) {
		raw["status"] = json.RawMessage(`"completed"`)
	})
	if _, err := store.Inspect(statusPlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "persisted status") {
		t.Fatalf("persisted status mismatch was accepted: %v", err)
	}

	phasePlan := testSyncJournalPlan(t)
	phaseHandle, err := store.Create(phasePlan)
	if err != nil {
		t.Fatal(err)
	}
	phaseLocation := phaseHandle.location
	if err := phaseHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, phaseLocation, func(raw map[string]json.RawMessage) {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["phase"] = json.RawMessage(`"teleported"`)
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})
	if _, err := store.Inspect(phasePlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "invalid phase") {
		t.Fatalf("unknown journal item phase was accepted: %v", err)
	}
}

func TestSyncJournalRejectsInconsistentItemAndRunStatsState(t *testing.T) {
	store := testSyncJournalStore(t)
	postPlan := testSyncJournalPlan(t)
	postHandle, err := store.Create(postPlan)
	if err != nil {
		t.Fatal(err)
	}
	postLocation := postHandle.location
	if err := postHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, postLocation, func(raw map[string]json.RawMessage) {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["postcondition"] = json.RawMessage(`{"side":"remote","exists":true,"kind":"file"}`)
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})
	if _, err := store.Inspect(postPlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "postcondition before successful completion") {
		t.Fatalf("premature postcondition was accepted: %v", err)
	}

	statsPlan := testSyncJournalPlan(t)
	statsHandle, err := store.Create(statsPlan)
	if err != nil {
		t.Fatal(err)
	}
	statsLocation := statsHandle.location
	if err := statsHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, statsLocation, func(raw map[string]json.RawMessage) {
		raw["run_stats"] = json.RawMessage(`{"runs":1,"resume_runs":2,"interrupted_runs":0,"last_duration_ms":0,"total_duration_ms":0}`)
	})
	if _, err := store.Inspect(statsPlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "exceed total runs") {
		t.Fatalf("inconsistent run statistics were accepted: %v", err)
	}
}

func TestSyncJournalRejectsNullRunStatsAndSucceededWithoutPostcondition(t *testing.T) {
	store := testSyncJournalStore(t)
	nullStatsPlan := testSyncJournalPlan(t)
	nullStatsHandle, err := store.Create(nullStatsPlan)
	if err != nil {
		t.Fatal(err)
	}
	nullStatsLocation := nullStatsHandle.location
	if err := nullStatsHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, nullStatsLocation, func(raw map[string]json.RawMessage) {
		raw["run_stats"] = json.RawMessage(`null`)
	})
	if _, err := store.Inspect(nullStatsPlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "run_stats") {
		t.Fatalf("null run_stats was accepted: %v", err)
	}

	succeededPlan := testSyncJournalPlan(t)
	succeededHandle, err := store.Create(succeededPlan)
	if err != nil {
		t.Fatal(err)
	}
	succeededLocation := succeededHandle.location
	if err := succeededHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, succeededLocation, func(raw map[string]json.RawMessage) {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["state"] = json.RawMessage(`"succeeded"`)
		items[0]["phase"] = json.RawMessage(`"done"`)
		delete(items[0], "postcondition")
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})
	if _, err := store.Inspect(succeededPlan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "without a postcondition") {
		t.Fatalf("succeeded action without postcondition was accepted: %v", err)
	}
}

func TestSyncJournalDoctorAuditsRequiredMigrationBackupsAsWarnings(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, _ := writeLegacySyncJournalV1(t, store, plan, nil)
	if _, err := store.Migrate(plan.PlanID); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Migrations) != 1 || !journal.Migrations[0].BackupRequired {
		t.Fatalf("migration did not require a backup: %#v", journal.Migrations)
	}
	backupPath, err := syncJournalMigrationBackupPath(location, journal.Migrations[0])
	if err != nil {
		t.Fatal(err)
	}

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	entry := findSyncJournalDoctorEntry(t, report, plan.PlanID)
	if entry.MigrationBackupStatus != syncJournalBackupOK || entry.MigrationBackupsRequired != 1 || report.Warnings != 0 {
		t.Fatalf("healthy migration backup diagnosis: entry=%#v report=%#v", entry, report)
	}

	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	report, err = store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	entry = findSyncJournalDoctorEntry(t, report, plan.PlanID)
	if entry.Health != syncJournalDoctorHealthy || entry.MigrationBackupStatus != syncJournalBackupMissing || entry.MigrationBackupsMissing != 1 || report.Issues != 0 || report.Warnings != 1 || report.MigrationBackupsMissing != 1 || !report.AllCurrentAndValid {
		t.Fatalf("missing backup should be a non-blocking audit warning: entry=%#v report=%#v", entry, report)
	}
}

func TestSyncJournalDoctorCommandIsReadOnlyAndAuthIndependent(t *testing.T) {
	if !commandSkipsAuthentication(syncJournalDoctorCmd) {
		t.Fatal("sync journal doctor unexpectedly requires authentication")
	}
	if !strings.Contains(syncJournalDoctorCmd.Long, "Read-only offline") || !strings.Contains(syncJournalDoctorCmd.Long, "without rewriting") || !strings.Contains(syncJournalDoctorCmd.Long, "exit non-zero") || !strings.Contains(syncJournalDoctorCmd.Long, "batch crash marker") {
		t.Fatalf("doctor help does not document read-only/fail-closed contract: %q", syncJournalDoctorCmd.Long)
	}
}
