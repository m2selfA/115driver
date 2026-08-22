package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func diagnoseMCPRecoveryForTest(t *testing.T, fixture mcpPersistentDeleteFixture) DiagnoseSyncRecoveryOutput {
	t.Helper()
	result, output, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("diagnose recovery result=%#v output=%#v err=%v", result, output, err)
	}
	if output.DiagnosisID == "" {
		t.Fatalf("diagnose recovery returned no diagnosis_id: %#v", output)
	}
	return output
}

func TestReconcileSyncRecoveryRetryFullThenExecuteResidual(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	diagnosis := diagnoseMCPRecoveryForTest(t, fixture)
	if !diagnosis.Resolvable || diagnosis.RetryFull != 1 {
		t.Fatalf("retry-full diagnosis = %#v", diagnosis)
	}

	result, output, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || result == nil || result.IsError || !output.Applied || !output.ResumeCandidate || output.RetryFull != 1 || output.DiagnosisID != diagnosis.DiagnosisID || output.JournalState != syncjournalpkg.StatusActive || output.JournalStatus != syncjournalpkg.StatusActive {
		t.Fatalf("retry-full reconcile result=%#v output=%#v err=%v", result, output, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].State != "pending" || journal.Items[0].Phase != syncjournalpkg.PhasePending || journal.Items[0].Attempts != 1 || journal.Items[0].Post != nil || journal.Items[0].LastError != "" {
		t.Fatalf("reconciled retry-full journal item = %#v", journal.Items[0])
	}
	if _, err := os.Stat(fixture.localPath); err != nil {
		t.Fatalf("reconciliation mutated local target: %v", err)
	}

	execResult, execOutput, execErr := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if execErr != nil || execResult == nil || execResult.IsError || execOutput.ErrorCode != "" || !execOutput.Summary.JournalResumed || execOutput.Summary.JournalCompletedBefore != 0 {
		t.Fatalf("post-reconcile residual execution result=%#v output=%#v err=%v", execResult, execOutput, execErr)
	}
	if _, err := os.Stat(fixture.localPath); !os.IsNotExist(err) {
		t.Fatalf("residual delete did not remove local target: %v", err)
	}
	journal, err = fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.Status != syncjournalpkg.StatusCompleted || journal.Items[0].State != "succeeded" || journal.Items[0].Attempts != 2 {
		t.Fatalf("post-reconcile completed journal = %#v", journal)
	}
}

func TestReconcileSyncRecoveryCompletedEvidenceRecordsPostconditionWithoutContentMutation(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	if err := os.Remove(fixture.localPath); err != nil {
		t.Fatal(err)
	}
	diagnosis := diagnoseMCPRecoveryForTest(t, fixture)
	if !diagnosis.Resolvable || diagnosis.Completed != 1 {
		t.Fatalf("completed diagnosis = %#v", diagnosis)
	}
	result, output, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || result == nil || result.IsError || !output.Applied || output.Completed != 1 || !output.ResumeCandidate {
		t.Fatalf("completed reconcile result=%#v output=%#v err=%v", result, output, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	item := journal.Items[0]
	if item.State != "succeeded" || item.Phase != syncjournalpkg.PhaseDone || item.Attempts != 1 || item.Post == nil || item.Post.Side != "local" || item.Post.Exists {
		t.Fatalf("completed recovery journal item = %#v", item)
	}
	if _, err := os.Stat(fixture.localPath); !os.IsNotExist(err) {
		t.Fatalf("completed reconciliation recreated/mutated local target: %v", err)
	}
}

func TestReconcileSyncRecoveryRejectsStaleDiagnosisWithoutJournalMutation(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	diagnosis := diagnoseMCPRecoveryForTest(t, fixture)
	before, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.localPath, []byte("changed-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || result == nil || !result.IsError || output.Applied || output.ErrorCode != "diagnosis_changed" || output.DiagnosisID != "" {
		t.Fatalf("stale diagnosis reconcile result=%#v output=%#v err=%v", result, output, err)
	}
	after, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || after.Items[0].State != before.Items[0].State || after.Items[0].Phase != before.Items[0].Phase || after.Items[0].Attempts != before.Items[0].Attempts || after.Items[0].LastError != before.Items[0].LastError || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("stale diagnosis mutated journal: before=%#v after=%#v", before, after)
	}
}

func TestReconcileSyncRecoveryRefusesAmbiguousExactDiagnosis(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	if err := os.WriteFile(fixture.localPath, []byte("changed-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnosis := diagnoseMCPRecoveryForTest(t, fixture)
	if diagnosis.Resolvable || diagnosis.Ambiguous != 1 {
		t.Fatalf("ambiguous diagnosis = %#v", diagnosis)
	}
	before, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: diagnosis.DiagnosisID,
	})
	if err != nil || result == nil || !result.IsError || output.Applied || output.ErrorCode != "recovery_ambiguous" {
		t.Fatalf("ambiguous reconcile result=%#v output=%#v err=%v", result, output, err)
	}
	after, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Items[0].State != before.Items[0].State || after.Items[0].Phase != before.Items[0].Phase || after.Items[0].Attempts != before.Items[0].Attempts || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("ambiguous diagnosis mutated journal: before=%#v after=%#v", before, after)
	}
}

func TestReconcileSyncRecoveryRefusesConcurrentJournal(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	handle := seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, true)
	defer handle.Close()
	result, output, err := fixture.ft.reconcileSyncRecovery(context.Background(), nil, ReconcileSyncRecoveryArgs{
		PlanID: fixture.args.ExpectPlanID, ExpectDiagnosisID: "sha256:" + strings.Repeat("a", 64),
	})
	if err != nil || result == nil || !result.IsError || output.Applied || output.ErrorCode != "journal_in_use" {
		t.Fatalf("concurrent recovery reconcile result=%#v output=%#v err=%v", result, output, err)
	}
}

func TestReconcileSyncRecoveryWireUsesReviewedTokenWithoutSecrets(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	diagnosis := diagnoseMCPRecoveryForTest(t, fixture)

	server := mcp.NewServer(&mcp.Implementation{Name: "reconcile-sync-recovery-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "reconcile_sync_recovery", Arguments: map[string]any{
		"plan_id": fixture.args.ExpectPlanID, "expect_diagnosis_id": diagnosis.DiagnosisID,
	}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire recovery reconciliation result=%#v err=%v", result, err)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output ReconcileSyncRecoveryOutput
	if err := json.Unmarshal(structured, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Applied || !output.ResumeCandidate || output.DiagnosisID != diagnosis.DiagnosisID || output.RetryFull != 1 {
		t.Fatalf("wire recovery reconciliation output=%#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath, "synthetic destructive interruption"} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("wire recovery reconciliation leaked %q: %s", forbidden, payload)
			}
		}
		lower := strings.ToLower(payload)
		if strings.Contains(lower, "sha1") || strings.Contains(lower, "postcondition") || strings.Contains(lower, "remote_id") {
			t.Fatalf("wire recovery reconciliation leaked hidden evidence: %s", payload)
		}
	}
}
