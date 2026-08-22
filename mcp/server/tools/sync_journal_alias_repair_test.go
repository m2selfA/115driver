package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func findMCPAliasDiagnostic(t *testing.T, output DiagnoseSyncJournalAliasesOutput, status string) MCPSyncJournalAliasDiagnostic {
	t.Helper()
	for _, item := range output.Items {
		if item.Status == status {
			return item
		}
	}
	t.Fatalf("alias diagnosis omitted status %q: %#v", status, output)
	return MCPSyncJournalAliasDiagnostic{}
}

func TestReconcileSyncJournalAliasRemovesOnlyReviewedOrphan(t *testing.T) {
	fixture, _, _ := prepareSyncJournalAliasDiagnosisFixture(t)
	_, diagnosis, err := fixture.ft.diagnoseSyncJournalAliases(context.Background(), nil, DiagnoseSyncJournalAliasesArgs{Limit: 10})
	if err != nil || diagnosis.ErrorCode != "" {
		t.Fatalf("diagnose orphan before repair=%#v err=%v", diagnosis, err)
	}
	orphan := findMCPAliasDiagnostic(t, diagnosis, string(syncjournalpkg.ReviewAliasDiagnosisOrphan))
	if orphan.RepairID == "" {
		t.Fatal("orphan diagnosis omitted repair_id")
	}
	result, output, err := fixture.ft.reconcileSyncJournalAlias(context.Background(), nil, ReconcileSyncJournalAliasArgs{
		PlanID: orphan.PlanID, ExpectRepairID: orphan.RepairID,
	})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || !output.Repaired || output.Status != "removed" || output.PlanID != orphan.PlanID || output.RepairID != orphan.RepairID {
		t.Fatalf("orphan reconcile result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.ResolveReviewAlias(orphan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("repaired orphan alias still exists: %v", err)
	}
	for _, item := range diagnosis.Items {
		if item.PlanID == orphan.PlanID {
			continue
		}
		if _, err := fixture.store.ResolveReviewAlias(item.PlanID); err != nil {
			t.Fatalf("repair mutated non-orphan alias %s: %v", item.PlanID, err)
		}
	}
}

func TestReconcileSyncJournalAliasWireKeepsRepairEvidenceOpaque(t *testing.T) {
	fixture, _, forbidden := prepareSyncJournalAliasDiagnosisFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "alias-repair-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "alias-repair-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	diagnoseResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "diagnose_sync_journal_aliases", Arguments: map[string]any{"limit": 10}})
	if err != nil || diagnoseResult == nil || diagnoseResult.IsError || diagnoseResult.StructuredContent == nil {
		t.Fatalf("wire alias diagnose result=%#v err=%v", diagnoseResult, err)
	}
	diagnoseEncoded, _ := json.Marshal(diagnoseResult.StructuredContent)
	var diagnosis DiagnoseSyncJournalAliasesOutput
	if err := json.Unmarshal(diagnoseEncoded, &diagnosis); err != nil {
		t.Fatal(err)
	}
	orphan := findMCPAliasDiagnostic(t, diagnosis, string(syncjournalpkg.ReviewAliasDiagnosisOrphan))

	repairResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "reconcile_sync_journal_alias",
		Arguments: map[string]any{"plan_id": orphan.PlanID, "expect_repair_id": orphan.RepairID},
	})
	if err != nil || repairResult == nil || repairResult.IsError || repairResult.StructuredContent == nil || len(repairResult.Content) != 1 {
		t.Fatalf("wire alias repair result=%#v err=%v", repairResult, err)
	}
	repairEncoded, _ := json.Marshal(repairResult.StructuredContent)
	var output ReconcileSyncJournalAliasOutput
	if err := json.Unmarshal(repairEncoded, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Repaired || output.PlanID != orphan.PlanID || output.RepairID != orphan.RepairID || output.Status != "removed" {
		t.Fatalf("wire alias repair output=%#v", output)
	}
	text := repairResult.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(repairEncoded), text} {
		for _, secret := range forbidden {
			if strings.Contains(payload, secret) {
				t.Fatalf("wire alias repair leaked %q: %s", secret, payload)
			}
		}
		lower := strings.ToLower(payload)
		for _, hiddenField := range []string{"raw_plan_id", "internal_plan_id", "journal_path", "trash_path", "account_id", "profile_scope", "remote_id", "sha1", "postcondition"} {
			if strings.Contains(lower, hiddenField) {
				t.Fatalf("wire alias repair leaked hidden field %q: %s", hiddenField, payload)
			}
		}
	}
	if _, err := fixture.store.ResolveReviewAlias(orphan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("wire alias repair left orphan alias: %v", err)
	}
}

func prepareMCPAliasRepairOrphan(t *testing.T) (mcpPersistentUploadFixture, syncplanpkg.Plan, MCPSyncJournalAliasDiagnostic) {
	t.Helper()
	fixture := newMCPPersistentUploadFixture(t)
	plan := fixture.state.Plan
	plan.RemoteRootID = "late-orphan-target"
	plan.PlanID = ""
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	envelope, err := buildMCPSyncPlanEnvelope(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(envelope.PlanID, plan.PlanID); err != nil {
		t.Fatal(err)
	}
	_, diagnosis, err := fixture.ft.diagnoseSyncJournalAliases(context.Background(), nil, DiagnoseSyncJournalAliasesArgs{Limit: 10})
	if err != nil || diagnosis.ErrorCode != "" {
		t.Fatalf("diagnose repair orphan=%#v err=%v", diagnosis, err)
	}
	for _, item := range diagnosis.Items {
		if item.PlanID == envelope.PlanID {
			if item.Status != string(syncjournalpkg.ReviewAliasDiagnosisOrphan) || item.RepairID == "" {
				t.Fatalf("repair orphan diagnosis=%#v", item)
			}
			return fixture, plan, item
		}
	}
	t.Fatalf("repair orphan %s missing from diagnosis: %#v", envelope.PlanID, diagnosis)
	return fixture, syncplanpkg.Plan{}, MCPSyncJournalAliasDiagnostic{}
}

func TestReconcileSyncJournalAliasRefusesOrphanThatBecameLive(t *testing.T) {
	fixture, plan, orphan := prepareMCPAliasRepairOrphan(t)
	handle, err := fixture.store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.reconcileSyncJournalAlias(context.Background(), nil, ReconcileSyncJournalAliasArgs{PlanID: orphan.PlanID, ExpectRepairID: orphan.RepairID})
	if err != nil || result == nil || !result.IsError || output.Repaired || output.ErrorCode != "alias_not_orphan" {
		t.Fatalf("late-live repair result=%#v output=%#v err=%v", result, output, err)
	}
	if resolved, err := fixture.store.ResolveReviewAlias(orphan.PlanID); err != nil || resolved != plan.PlanID {
		t.Fatalf("late-live repair mutated alias: resolved=%q err=%v", resolved, err)
	}
	if current, err := fixture.store.InspectCurrent(plan.PlanID); err != nil || current.PlanID != plan.PlanID {
		t.Fatalf("late-live repair changed current journal: current=%#v err=%v", current, err)
	}
}

func TestReconcileSyncJournalAliasRefusesOrphanThatBecameTrashed(t *testing.T) {
	fixture, plan, orphan := prepareMCPAliasRepairOrphan(t)
	handle, err := fixture.store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	location, err := fixture.store.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncjournalpkg.MoveDirectoryToSessionTrash(fixture.store.Root, location.Dir, plan.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.reconcileSyncJournalAlias(context.Background(), nil, ReconcileSyncJournalAliasArgs{PlanID: orphan.PlanID, ExpectRepairID: orphan.RepairID})
	if err != nil || result == nil || !result.IsError || output.Repaired || output.ErrorCode != "journal_trashed" {
		t.Fatalf("late-trash repair result=%#v output=%#v err=%v", result, output, err)
	}
	if resolved, err := fixture.store.ResolveReviewAlias(orphan.PlanID); err != nil || resolved != plan.PlanID {
		t.Fatalf("late-trash repair mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileSyncJournalAliasRejectsStaleTokenWithoutMutation(t *testing.T) {
	fixture, _, _ := prepareSyncJournalAliasDiagnosisFixture(t)
	_, diagnosis, err := fixture.ft.diagnoseSyncJournalAliases(context.Background(), nil, DiagnoseSyncJournalAliasesArgs{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	orphan := findMCPAliasDiagnostic(t, diagnosis, string(syncjournalpkg.ReviewAliasDiagnosisOrphan))
	wrong := "sha256:" + strings.Repeat("a", 64)
	if wrong == orphan.RepairID {
		wrong = "sha256:" + strings.Repeat("b", 64)
	}
	result, output, err := fixture.ft.reconcileSyncJournalAlias(context.Background(), nil, ReconcileSyncJournalAliasArgs{PlanID: orphan.PlanID, ExpectRepairID: wrong})
	if err != nil || result == nil || !result.IsError || output.Repaired || output.ErrorCode != "repair_changed" || output.RepairID != wrong {
		t.Fatalf("stale repair result=%#v output=%#v err=%v", result, output, err)
	}
	if resolved, err := fixture.store.ResolveReviewAlias(orphan.PlanID); err != nil || resolved == "" {
		t.Fatalf("stale token mutated orphan alias: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileSyncJournalAliasRefusesSoftDeletedAndIdentityMismatch(t *testing.T) {
	fixture, _, _ := prepareSyncJournalAliasDiagnosisFixture(t)
	_, diagnosis, err := fixture.ft.diagnoseSyncJournalAliases(context.Background(), nil, DiagnoseSyncJournalAliasesArgs{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	orphan := findMCPAliasDiagnostic(t, diagnosis, string(syncjournalpkg.ReviewAliasDiagnosisOrphan))
	for _, status := range []string{string(syncjournalpkg.ReviewAliasDiagnosisSoftDeleted), string(syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch)} {
		item := findMCPAliasDiagnostic(t, diagnosis, status)
		result, output, err := fixture.ft.reconcileSyncJournalAlias(context.Background(), nil, ReconcileSyncJournalAliasArgs{PlanID: item.PlanID, ExpectRepairID: orphan.RepairID})
		if err != nil || result == nil || !result.IsError || output.Repaired {
			t.Fatalf("unsafe status %s reconcile result=%#v output=%#v err=%v", status, result, output, err)
		}
		wantCode := "journal_trashed"
		if status == string(syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch) {
			wantCode = "journal_alias_conflict"
		}
		if output.ErrorCode != wantCode {
			t.Fatalf("unsafe status %s error_code=%q want=%q", status, output.ErrorCode, wantCode)
		}
		if _, err := fixture.store.ResolveReviewAlias(item.PlanID); err != nil {
			t.Fatalf("unsafe status %s alias was mutated: %v", status, err)
		}
	}
}
