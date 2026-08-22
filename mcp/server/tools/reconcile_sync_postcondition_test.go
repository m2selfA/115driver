package tools

import (
	"context"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
)

func seedMCPUploadMutationDoneJournal(t *testing.T, fixture mcpPersistentUploadFixture) {
	t.Helper()
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = syncjournalpkg.PhaseMutationDone
		journal.Items[0].Attempts = 1
		journal.Items[0].LastError = "postcondition was not persisted"
		now := time.Now().UTC()
		journal.RunStats.Runs = 1
		journal.RunStats.LastStartedAt = &now
		journal.RunStats.LastFinishedAt = &now
		return nil
	}); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if got := handle.Snapshot().Status; got != syncjournalpkg.StatusReconcileRequired {
		_ = handle.Close()
		t.Fatalf("mutation-done journal status = %q, want %q", got, syncjournalpkg.StatusReconcileRequired)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileSyncRecoveryObservesCompletedNonDestructiveMutationWithoutReplay(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	seedMCPUploadMutationDoneJournal(t, fixture)
	*fixture.uploaded = true // data path completed before the postcondition journal write

	diagnoseResult, diagnosis, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || diagnoseResult == nil || diagnoseResult.IsError || !diagnosis.Found || !diagnosis.ReconcileRequired || !diagnosis.EvidenceComplete || !diagnosis.Resolvable || diagnosis.DiagnosisID == "" || diagnosis.Checked != 1 || diagnosis.Completed != 1 || diagnosis.PendingObservation != 0 || len(diagnosis.Items) != 1 || diagnosis.Items[0].Decision != string(syncjournalpkg.DestructiveCompleted) {
		t.Fatalf("non-destructive completed diagnosis result=%#v output=%#v err=%v", diagnoseResult, diagnosis, err)
	}
	if *fixture.uploadCalls != 0 {
		t.Fatalf("diagnosis replayed upload: calls=%d", *fixture.uploadCalls)
	}

	reconcileResult, reconciled, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || reconcileResult == nil || reconcileResult.IsError || !reconciled.Applied || !reconciled.ResumeCandidate || reconciled.Completed != 1 || reconciled.PendingObservation != 0 || reconciled.JournalState != syncjournalpkg.StatusActive || reconciled.JournalStatus != syncjournalpkg.StatusActive {
		t.Fatalf("non-destructive completed reconcile result=%#v output=%#v err=%v", reconcileResult, reconciled, err)
	}
	if *fixture.uploadCalls != 0 {
		t.Fatalf("reconciliation replayed upload: calls=%d", *fixture.uploadCalls)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	item := journal.Items[0]
	if item.State != "succeeded" || item.Phase != syncjournalpkg.PhaseDone || item.Attempts != 1 || item.Post == nil || item.Post.Side != "remote" || !item.Post.Exists || item.Post.Kind != "file" {
		t.Fatalf("observed completion journal item = %#v", item)
	}

	executeResult, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || executeResult == nil || executeResult.IsError || output.ErrorCode != "" || !output.Summary.JournalResumed || output.Summary.JournalCompletedBefore != 1 {
		t.Fatalf("postcondition-complete residual execution result=%#v output=%#v err=%v", executeResult, output, err)
	}
	if *fixture.uploadCalls != 0 {
		t.Fatalf("residual execution replayed completed upload: calls=%d", *fixture.uploadCalls)
	}
	journal, err = fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.Status != syncjournalpkg.StatusCompleted || journal.Items[0].Attempts != 1 {
		t.Fatalf("postcondition-complete final journal = %#v", journal)
	}
}

func TestReconcileSyncRecoveryPendingObservationNeverReplaysOrAutoAdvances(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	seedMCPUploadMutationDoneJournal(t, fixture)

	diagnoseResult, diagnosis, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || diagnoseResult == nil || diagnoseResult.IsError || !diagnosis.Found || !diagnosis.ReconcileRequired || !diagnosis.EvidenceComplete || diagnosis.Resolvable || diagnosis.DiagnosisID == "" || diagnosis.PendingObservation != 1 || diagnosis.Ambiguous != 0 || len(diagnosis.Items) != 1 || diagnosis.Items[0].Decision != mcpSyncRecoveryPendingObservation {
		t.Fatalf("pending observation diagnosis result=%#v output=%#v err=%v", diagnoseResult, diagnosis, err)
	}
	before, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}

	reconcileResult, reconciled, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || reconcileResult == nil || !reconcileResult.IsError || reconciled.Applied || reconciled.ErrorCode != "postcondition_pending" || reconciled.DiagnosisID != diagnosis.DiagnosisID {
		t.Fatalf("pending observation reconcile result=%#v output=%#v err=%v", reconcileResult, reconciled, err)
	}
	after, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.State != before.State || after.Status != before.Status || after.Items[0].State != before.Items[0].State || after.Items[0].Phase != before.Items[0].Phase || after.Items[0].Attempts != before.Items[0].Attempts {
		t.Fatalf("pending observation changed journal: before=%#v after=%#v", before, after)
	}
	if *fixture.uploadCalls != 0 || *fixture.uploaded {
		t.Fatalf("pending observation replayed upload: calls=%d uploaded=%v", *fixture.uploadCalls, *fixture.uploaded)
	}

	*fixture.uploaded = true // outcome becomes observable only after the reviewed diagnosis
	staleResult, staleOutput, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || staleResult == nil || !staleResult.IsError || staleOutput.Applied || staleOutput.ErrorCode != "diagnosis_changed" || staleOutput.DiagnosisID != "" {
		t.Fatalf("stale pending token reconcile result=%#v output=%#v err=%v", staleResult, staleOutput, err)
	}
	finalJournal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !finalJournal.UpdatedAt.Equal(before.UpdatedAt) || finalJournal.Items[0].State != before.Items[0].State || finalJournal.Items[0].Phase != before.Items[0].Phase || finalJournal.Items[0].Attempts != before.Items[0].Attempts || *fixture.uploadCalls != 0 {
		t.Fatalf("stale pending token mutated state: journal=%#v calls=%d", finalJournal, *fixture.uploadCalls)
	}
}
