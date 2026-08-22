package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func testSyncDestructiveJournalPlan(t *testing.T) syncPlan {
	t.Helper()
	plan := testSyncJournalPlan(t)
	item := &plan.Items[0]
	item.Action = "delete-remote"
	item.Reason = "mirror-delete:remote-only"
	item.LocalPresent = false
	item.RemotePresent = true
	item.RemotePath = "/remote/old.bin"
	item.RemoteID = "old-id"
	item.RemoteSize = 4
	item.RemoteSHA1 = testSyncSHA1("old!")
	item.Destructive = true
	plan.Direction = syncDirectionUpload
	plan.DeleteExtraneous = true
	plan.DestructiveActions = 1
	plan.ChangeActions = 1
	plan.PlanID = syncPlanFingerprint(plan)
	return plan
}

func TestSyncJournalEffectiveStatusContract(t *testing.T) {
	plan := testSyncJournalPlan(t)
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("a", 64), 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := syncJournalEffectiveStatus(journal); got != "active" {
		t.Fatalf("new journal status: got %q want active", got)
	}
	journal.State = "failed"
	if got := syncJournalEffectiveStatus(journal); got != "failed" {
		t.Fatalf("failed journal status: got %q want failed", got)
	}

	destructivePlan := testSyncDestructiveJournalPlan(t)
	destructive, err := newSyncExecutionJournal(destructivePlan, strings.Repeat("b", 64), 42)
	if err != nil {
		t.Fatal(err)
	}
	destructive.State = "failed"
	destructive.Items[0].State = "failed"
	destructive.Items[0].Phase = "delete-started"
	if got := syncJournalEffectiveStatus(destructive); got != "reconcile-required" {
		t.Fatalf("interrupted destructive status: got %q want reconcile-required", got)
	}
	destructive.State = "recovery-required"
	if got := syncJournalEffectiveStatus(destructive); got != "recovery-required" {
		t.Fatalf("manual recovery status: got %q want recovery-required", got)
	}
}

func TestSyncJournalInspectAndListExposeEffectiveStatus(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncDestructiveJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "delete-started"
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != "failed" || journal.Status != "reconcile-required" || journal.Version != syncJournalVersion || journal.MigrationRequired {
		t.Fatalf("inspect state/status/schema contract: %#v", journal)
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"state":"failed"`) || !strings.Contains(string(encoded), `"status":"reconcile-required"`) || strings.Contains(string(encoded), `"migration_required":true`) {
		t.Fatalf("inspect JSON violates state/status/schema contract: %s", encoded)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != "failed" || entries[0].Status != "reconcile-required" || !entries[0].ReconcileRequired {
		t.Fatalf("list state/status contract: %#v", entries)
	}
	entry := entries[0]
	if entry.Schema != syncJournalListEntrySchema || entry.Total != 1 || entry.Completed != 0 || entry.Pending != 0 || entry.Failed != 1 || entry.Blocked != 0 || entry.StaleForMillis < 0 {
		t.Fatalf("list aggregate counters/staleness contract: %#v", entry)
	}
	if entry.ActionCounts["delete-remote"] != 1 || entry.StateCounts["failed"] != 1 || entry.PhaseCounts["delete-started"] != 1 {
		t.Fatalf("list action/state/phase counters: %#v", entry)
	}
	listJSON, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"schema":"` + syncJournalListEntrySchema + `"`, `"stale_for_ms"`, `"total"`, `"action_counts"`, `"state_counts"`, `"phase_counts"`} {
		if !strings.Contains(string(listJSON), key) {
			t.Fatalf("list JSON missing observability field %s: %s", key, listJSON)
		}
	}
}

func TestSyncJournalListCounterFormattingIsDeterministic(t *testing.T) {
	if got := syncJournalCountKey(""); got != "unset" {
		t.Fatalf("empty counter key: %q", got)
	}
	if got := formatSyncJournalCountMap(map[string]int{"z": 1, "a": 2}); got != "a:2,z:1" {
		t.Fatalf("counter map formatting is not deterministic: %q", got)
	}
	if got := formatSyncJournalStaleAge(1500); got != "1s" {
		t.Fatalf("stale age formatting: %q", got)
	}
}

func TestSyncJournalRecoveryResultMachineSchema(t *testing.T) {
	before := syncExecutionJournal{PlanID: "plan", Version: syncJournalVersion, State: syncJournalStatusRecoveryRequired, Status: syncJournalStatusRecoveryRequired}
	after := before
	after.State = syncJournalStatusActive
	after.Status = syncJournalStatusActive
	verification := syncJournalVerification{Schema: syncJournalVerificationSchema, PlanID: "plan", RecoveryClearable: true}
	result := newSyncJournalRecoveryResult(before, after, verification)
	if result.Schema != syncJournalRecoveryResultSchema || result.Verification.Schema != syncJournalVerificationSchema || result.PreviousState != syncJournalStatusRecoveryRequired || result.State != syncJournalStatusActive {
		t.Fatalf("recovery result schema/state contract: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"schema":"`+syncJournalRecoveryResultSchema+`"`) || !strings.Contains(string(encoded), `"verification":{"schema":"`+syncJournalVerificationSchema+`"`) {
		t.Fatalf("recovery JSON schema contract: %s", encoded)
	}
}
