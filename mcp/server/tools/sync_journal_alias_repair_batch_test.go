package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prepareMCPAliasRepairBatchFixture(t *testing.T) (mcpPersistentUploadFixture, []string, []string) {
	t.Helper()
	fixture, _, forbidden := prepareSyncJournalAliasDiagnosisFixture(t)
	secondReview := "sha256:" + strings.Repeat("c", 64)
	secondRaw := strings.Repeat("7", 64)
	if _, err := fixture.store.WriteReviewAlias(secondReview, secondRaw); err != nil {
		t.Fatal(err)
	}
	return fixture, []string{secondReview, "sha256:" + strings.Repeat("d", 64)}, forbidden
}

func TestPlanSyncJournalAliasRepairSelectsOnlyOrphansAndBindsCompleteSet(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	output, selected, err := planMCPSyncJournalAliasRepair(fixture.store, 1)
	if err != nil {
		t.Fatal(err)
	}
	if output.Eligible != 2 || output.Selected != 1 || len(output.Items) != 1 || len(selected) != 1 || output.RepairSetID == "" {
		t.Fatalf("batch alias repair preview=%#v selected=%#v", output, selected)
	}
	if output.Items[0].PlanID != orphanIDs[0] || output.Items[0].RepairID == "" {
		t.Fatalf("batch alias repair selected unexpected orphan: %#v", output.Items[0])
	}
	for _, item := range output.Items {
		if item.PlanID == fixture.args.ExpectPlanID {
			t.Fatalf("live alias entered repair candidate set: %#v", output)
		}
	}

	// Change the unselected orphan. Because repair_set_id binds the complete
	// orphan set rather than only the first selected item, the token must change.
	unselectedReview := orphanIDs[1]
	rawID, err := fixture.store.ResolveReviewAlias(unselectedReview)
	if err != nil {
		t.Fatal(err)
	}
	scan, err := fixture.store.DiagnoseReviewAliases(maxMCPSyncJournalScan, maxMCPSyncJournalScan, nil)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot syncjournalpkg.ReviewAlias
	for _, entry := range scan.Entries {
		if entry.Alias.ReviewID == unselectedReview {
			snapshot = entry.Alias
			break
		}
	}
	if snapshot.ReviewID == "" {
		t.Fatalf("unselected orphan missing from shared diagnosis")
	}
	if removed, err := fixture.store.RemoveOrphanReviewAliasExact(snapshot); err != nil || !removed {
		t.Fatalf("refresh unselected orphan remove=%v err=%v", removed, err)
	}
	if _, err := fixture.store.WriteReviewAlias(unselectedReview, rawID); err != nil {
		t.Fatal(err)
	}
	changed, _, err := planMCPSyncJournalAliasRepair(fixture.store, 1)
	if err != nil {
		t.Fatal(err)
	}
	if changed.RepairSetID == output.RepairSetID {
		t.Fatalf("unselected orphan change did not invalidate repair_set_id: %q", output.RepairSetID)
	}
}

func TestExecuteSyncJournalAliasRepairRejectsStaleSetBeforeFirstRemoval(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	preview, _, err := planMCPSyncJournalAliasRepair(fixture.store, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Change only the unselected orphan after review.
	rawID, err := fixture.store.ResolveReviewAlias(orphanIDs[1])
	if err != nil {
		t.Fatal(err)
	}
	scan, err := fixture.store.DiagnoseReviewAliases(maxMCPSyncJournalScan, maxMCPSyncJournalScan, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range scan.Entries {
		if entry.Alias.ReviewID != orphanIDs[1] {
			continue
		}
		if removed, err := fixture.store.RemoveOrphanReviewAliasExact(entry.Alias); err != nil || !removed {
			t.Fatalf("change unselected orphan remove=%v err=%v", removed, err)
		}
		break
	}
	if _, err := fixture.store.WriteReviewAlias(orphanIDs[1], rawID); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 1, ExpectRepairSetID: preview.RepairSetID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "repair_changed" || output.Requested != 0 || output.Removed != 0 || len(output.Items) != 0 {
		t.Fatalf("stale batch repair result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.ResolveReviewAlias(orphanIDs[0]); err != nil {
		t.Fatalf("stale repair removed selected orphan before set mismatch: %v", err)
	}
}

func TestExecuteSyncJournalAliasRepairChangedSetDoesNotExposeReplacementCandidates(t *testing.T) {
	fixture, _, _ := prepareMCPAliasRepairBatchFixture(t)
	preview, selected, err := planMCPSyncJournalAliasRepair(fixture.store, 1)
	if err != nil || len(selected) != 1 {
		t.Fatalf("initial batch preview=%#v selected=%#v err=%v", preview, selected, err)
	}
	if removed, err := fixture.store.RemoveOrphanReviewAliasExact(selected[0]); err != nil || !removed {
		t.Fatalf("remove originally selected orphan=%v err=%v", removed, err)
	}
	newReviewID := "sha256:" + strings.Repeat("0", 64)
	if _, err := fixture.store.WriteReviewAlias(newReviewID, strings.Repeat("6", 64)); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := planMCPSyncJournalAliasRepair(fixture.store, 1)
	if err != nil || len(fresh.Items) != 1 || fresh.Items[0].PlanID != newReviewID || fresh.RepairSetID == preview.RepairSetID {
		t.Fatalf("fresh replacement preview=%#v err=%v", fresh, err)
	}

	result, output, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 1, ExpectRepairSetID: preview.RepairSetID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "repair_changed" || output.RepairSetID != preview.RepairSetID || output.Requested != 0 || output.Removed != 0 || len(output.Items) != 0 {
		t.Fatalf("changed-set redaction result=%#v output=%#v err=%v", result, output, err)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, newReviewID) || strings.Contains(payload, fresh.RepairSetID) || strings.Contains(payload, fresh.Items[0].RepairID) {
			t.Fatalf("changed-set error leaked replacement review state: %s", payload)
		}
	}
	if _, err := fixture.store.ResolveReviewAlias(newReviewID); err != nil {
		t.Fatalf("changed-set execution mutated replacement candidate: %v", err)
	}
}

func TestExecuteSyncJournalAliasRepairCrashStateRequiresFreshPlan(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	preview, selected, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil || len(selected) != 2 {
		t.Fatalf("initial MCP crash-state preview=%#v selected=%#v err=%v", preview, selected, err)
	}
	if removed, err := fixture.store.RemoveOrphanReviewAliasExact(selected[0]); err != nil || !removed {
		t.Fatalf("simulate MCP post-delete crash remove=%v err=%v", removed, err)
	}
	fresh, freshSelected, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil || fresh.RepairSetID == preview.RepairSetID || len(freshSelected) != 1 || len(fresh.Items) != 1 {
		t.Fatalf("fresh MCP crash-state preview=%#v selected=%#v err=%v", fresh, freshSelected, err)
	}

	result, output, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 2, ExpectRepairSetID: preview.RepairSetID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "repair_changed" || output.RepairSetID != preview.RepairSetID || output.Requested != 0 || output.Removed != 0 || output.Unchanged != 0 || output.Unknown != 0 || output.Partial || output.RecoveryRequired || len(output.Items) != 0 {
		t.Fatalf("stale MCP post-crash execution result=%#v output=%#v err=%v", result, output, err)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, fresh.RepairSetID) || strings.Contains(payload, fresh.Items[0].RepairID) || strings.Contains(payload, fresh.Items[0].PlanID) {
			t.Fatalf("stale MCP post-crash execution leaked fresh review state: %s", payload)
		}
	}
	if _, err := fixture.store.ResolveReviewAlias(orphanIDs[1]); err != nil {
		t.Fatalf("stale MCP post-crash execution mutated remaining orphan: %v", err)
	}

	result, output, err = fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 2, ExpectRepairSetID: fresh.RepairSetID})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Requested != 1 || output.Removed != 1 || output.Unchanged != 0 || output.Unknown != 0 || output.Partial || output.RecoveryRequired || len(output.Items) != 1 || output.Items[0].Status != "removed" {
		t.Fatalf("fresh MCP post-crash execution result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.ResolveReviewAlias(orphanIDs[1]); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("fresh MCP post-crash execution left remaining orphan: %v", err)
	}
}

func TestExecuteSyncJournalAliasRepairAuthenticatedAccountMismatchFailsClosed(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	ownerPreview, _, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil || ownerPreview.Selected != 2 {
		t.Fatalf("owner MCP repair preview=%#v err=%v", ownerPreview, err)
	}
	foreign := fixture.store
	foreign.AccountID = fixture.store.AccountID + 1
	foreignReviewID := "sha256:" + strings.Repeat("2", 64)
	foreignRawID := strings.Repeat("3", 64)
	foreignAlias, err := foreign.WriteReviewAlias(foreignReviewID, foreignRawID)
	if err != nil {
		t.Fatal(err)
	}

	planResult, planOutput, err := fixture.ft.planSyncJournalAliasRepair(context.Background(), nil, PlanSyncJournalAliasRepairArgs{Limit: 2})
	if err != nil || planResult == nil || !planResult.IsError || planOutput.ErrorCode != "journal_alias_diagnosis_failed" || planOutput.RepairSetID != "" || planOutput.Scanned != 0 || planOutput.Selected != 0 || len(planOutput.Items) != 0 {
		t.Fatalf("foreign-account MCP plan did not fail closed: result=%#v output=%#v err=%v", planResult, planOutput, err)
	}
	executeResult, executeOutput, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 2, ExpectRepairSetID: ownerPreview.RepairSetID})
	if err != nil || executeResult == nil || !executeResult.IsError || executeOutput.ErrorCode != "journal_alias_diagnosis_failed" || executeOutput.RepairSetID != ownerPreview.RepairSetID || executeOutput.Requested != 0 || executeOutput.Removed != 0 || executeOutput.Unchanged != 0 || executeOutput.Unknown != 0 || executeOutput.Partial || executeOutput.RecoveryRequired || len(executeOutput.Items) != 0 {
		t.Fatalf("foreign-account MCP execute did not fail closed: result=%#v output=%#v err=%v", executeResult, executeOutput, err)
	}
	encodedPlan, _ := json.Marshal(planOutput)
	encodedExecute, _ := json.Marshal(executeOutput)
	for _, payload := range []string{string(encodedPlan), string(encodedExecute), executeResult.Content[0].(*mcp.TextContent).Text} {
		if strings.Contains(payload, foreignReviewID) || strings.Contains(payload, foreignRawID) {
			t.Fatalf("authenticated MCP account mismatch leaked foreign alias state: %s", payload)
		}
	}
	for _, reviewID := range orphanIDs {
		if _, err := fixture.store.ResolveReviewAlias(reviewID); err != nil {
			t.Fatalf("foreign-account MCP failure mutated owner alias %s: %v", reviewID, err)
		}
	}
	if resolved, err := foreign.ResolveReviewAlias(foreignReviewID); err != nil || resolved != foreignRawID {
		t.Fatalf("foreign-account MCP failure mutated foreign alias: resolved=%q err=%v", resolved, err)
	}
	if removed, err := foreign.RemoveOrphanReviewAliasExact(foreignAlias); err != nil || !removed {
		t.Fatalf("foreign owner could not clean its own test alias: removed=%v err=%v", removed, err)
	}
}

func TestExecuteSyncJournalAliasRepairRemovesSelectedOrphansOnly(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	preview, _, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 2, ExpectRepairSetID: preview.RepairSetID})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Removed != 2 || output.Requested != 2 || output.Unchanged != 0 || output.Unknown != 0 || output.Partial || output.RecoveryRequired {
		t.Fatalf("batch repair result=%#v output=%#v err=%v", result, output, err)
	}
	for _, reviewID := range orphanIDs {
		if _, err := fixture.store.ResolveReviewAlias(reviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
			t.Fatalf("orphan alias %s survived batch repair: %v", reviewID, err)
		}
	}
	if _, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); err != nil {
		t.Fatalf("batch repair removed live alias: %v", err)
	}
}

func TestExecuteSyncJournalAliasRepairLockContentionRemovesNoCandidate(t *testing.T) {
	fixture, orphanIDs, _ := prepareMCPAliasRepairBatchFixture(t)
	preview, _, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil {
		t.Fatal(err)
	}
	rawID, err := fixture.store.ResolveReviewAlias(orphanIDs[1])
	if err != nil {
		t.Fatal(err)
	}
	location, err := fixture.store.Location(rawID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, output, err := fixture.ft.executeSyncJournalAliasRepair(context.Background(), nil, ExecuteSyncJournalAliasRepairArgs{Limit: 2, ExpectRepairSetID: preview.RepairSetID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_in_use" || output.RepairSetID != preview.RepairSetID || output.Requested != 2 || output.Removed != 0 || output.Unchanged != 2 || output.Unknown != 0 || output.Partial || output.RecoveryRequired || len(output.Items) != 2 {
		t.Fatalf("locked batch repair result=%#v output=%#v err=%v", result, output, err)
	}
	for _, item := range output.Items {
		if item.Status != "unchanged" {
			t.Fatalf("locked MCP batch item status=%q", item.Status)
		}
	}
	for _, reviewID := range orphanIDs {
		if _, err := fixture.store.ResolveReviewAlias(reviewID); err != nil {
			t.Fatalf("lock contention removed candidate %s: %v", reviewID, err)
		}
	}
}

func TestAliasRepairExecutionErrorDistinguishesRollbackFromRecovery(t *testing.T) {
	selected := []syncjournalpkg.ReviewAlias{
		{ReviewID: "sha256:" + strings.Repeat("1", 64)},
		{ReviewID: "sha256:" + strings.Repeat("2", 64)},
	}
	expectedID := "sha256:" + strings.Repeat("3", 64)

	result, output, err := aliasRepairExecutionError(expectedID, "repair_failed", "rolled back", 2, selected, syncjournalpkg.ReviewAliasBatchRemovalResult{Requested: 2, RolledBack: true})
	if err != nil || result == nil || !result.IsError || output.Removed != 0 || output.Unchanged != 2 || output.Unknown != 0 || output.Partial || output.RecoveryRequired {
		t.Fatalf("rolled-back output=%#v result=%#v err=%v", output, result, err)
	}
	for _, item := range output.Items {
		if item.Status != "unchanged" {
			t.Fatalf("rolled-back item status=%q", item.Status)
		}
	}

	result, output, err = aliasRepairExecutionError(expectedID, "recovery_required", "rollback failed", 2, selected, syncjournalpkg.ReviewAliasBatchRemovalResult{Requested: 2, Removed: 1, RecoveryRequired: true})
	if err != nil || result == nil || !result.IsError || output.Removed != 1 || output.Unchanged != 0 || output.Unknown != 2 || !output.Partial || !output.RecoveryRequired {
		t.Fatalf("recovery-required output=%#v result=%#v err=%v", output, result, err)
	}
	for _, item := range output.Items {
		if item.Status != "unknown" {
			t.Fatalf("recovery-required item status=%q", item.Status)
		}
	}
}

func TestSyncJournalAliasRepairBatchWireKeepsHiddenIdentityOpaque(t *testing.T) {
	fixture, _, forbidden := prepareMCPAliasRepairBatchFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "alias-repair-batch-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "alias-repair-batch-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	planResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "plan_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2}})
	if err != nil || planResult == nil || planResult.IsError || planResult.StructuredContent == nil {
		t.Fatalf("wire batch alias repair plan result=%#v err=%v", planResult, err)
	}
	encodedPlan, _ := json.Marshal(planResult.StructuredContent)
	var preview PlanSyncJournalAliasRepairOutput
	if err := json.Unmarshal(encodedPlan, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Selected != 2 || preview.RepairSetID == "" {
		t.Fatalf("wire batch alias repair preview=%#v", preview)
	}

	executeResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2, "expect_repair_set_id": preview.RepairSetID}})
	if err != nil || executeResult == nil || executeResult.IsError || executeResult.StructuredContent == nil || len(executeResult.Content) != 1 {
		t.Fatalf("wire batch alias repair execute result=%#v err=%v", executeResult, err)
	}
	encodedExecute, _ := json.Marshal(executeResult.StructuredContent)
	text := executeResult.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encodedPlan), string(encodedExecute), text} {
		for _, secret := range forbidden {
			if strings.Contains(payload, secret) {
				t.Fatalf("wire batch alias repair leaked %q: %s", secret, payload)
			}
		}
		lower := strings.ToLower(payload)
		for _, hiddenField := range []string{"raw_plan_id", "internal_plan_id", "journal_path", "trash_path", "account_id", "profile_scope", "remote_id", "sha1", "postcondition"} {
			if strings.Contains(lower, hiddenField) {
				t.Fatalf("wire batch alias repair leaked hidden field %q: %s", hiddenField, payload)
			}
		}
	}
}

func TestSyncJournalAliasRepairBatchWireRequiresFreshPlanAfterCrashState(t *testing.T) {
	fixture, _, _ := prepareMCPAliasRepairBatchFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "alias-repair-crash-wire-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "alias-repair-crash-wire-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	planResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "plan_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2}})
	if err != nil || planResult == nil || planResult.IsError || planResult.StructuredContent == nil {
		t.Fatalf("wire crash-state initial plan result=%#v err=%v", planResult, err)
	}
	encodedPlan, _ := json.Marshal(planResult.StructuredContent)
	var preview PlanSyncJournalAliasRepairOutput
	if err := json.Unmarshal(encodedPlan, &preview); err != nil {
		t.Fatal(err)
	}
	_, selected, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil || len(selected) != 2 || preview.RepairSetID == "" {
		t.Fatalf("wire crash-state selected=%#v preview=%#v err=%v", selected, preview, err)
	}
	if removed, err := fixture.store.RemoveOrphanReviewAliasExact(selected[0]); err != nil || !removed {
		t.Fatalf("wire crash-state partial delete remove=%v err=%v", removed, err)
	}
	freshDirect, _, err := planMCPSyncJournalAliasRepair(fixture.store, 2)
	if err != nil || freshDirect.RepairSetID == preview.RepairSetID || len(freshDirect.Items) != 1 {
		t.Fatalf("wire crash-state fresh direct preview=%#v err=%v", freshDirect, err)
	}

	staleResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2, "expect_repair_set_id": preview.RepairSetID}})
	if err != nil || staleResult == nil || !staleResult.IsError || staleResult.StructuredContent == nil || len(staleResult.Content) != 1 {
		t.Fatalf("wire crash-state stale execute result=%#v err=%v", staleResult, err)
	}
	encodedStale, _ := json.Marshal(staleResult.StructuredContent)
	var stale ExecuteSyncJournalAliasRepairOutput
	if err := json.Unmarshal(encodedStale, &stale); err != nil {
		t.Fatal(err)
	}
	if stale.ErrorCode != "repair_changed" || stale.RepairSetID != preview.RepairSetID || stale.Requested != 0 || stale.Removed != 0 || stale.Unchanged != 0 || stale.Unknown != 0 || stale.Partial || stale.RecoveryRequired || len(stale.Items) != 0 {
		t.Fatalf("wire crash-state stale output=%#v", stale)
	}
	staleText := staleResult.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encodedStale), staleText} {
		if !strings.Contains(payload, `"items":[]`) || strings.Contains(payload, freshDirect.RepairSetID) || strings.Contains(payload, freshDirect.Items[0].RepairID) || strings.Contains(payload, freshDirect.Items[0].PlanID) {
			t.Fatalf("wire crash-state stale payload violated review boundary: %s", payload)
		}
	}

	freshPlanResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "plan_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2}})
	if err != nil || freshPlanResult == nil || freshPlanResult.IsError || freshPlanResult.StructuredContent == nil {
		t.Fatalf("wire crash-state fresh plan result=%#v err=%v", freshPlanResult, err)
	}
	encodedFresh, _ := json.Marshal(freshPlanResult.StructuredContent)
	var fresh PlanSyncJournalAliasRepairOutput
	if err := json.Unmarshal(encodedFresh, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.RepairSetID != freshDirect.RepairSetID || fresh.Selected != 1 || len(fresh.Items) != 1 {
		t.Fatalf("wire crash-state fresh plan=%#v direct=%#v", fresh, freshDirect)
	}
	freshExecute, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_sync_journal_alias_repair", Arguments: map[string]any{"limit": 2, "expect_repair_set_id": fresh.RepairSetID}})
	if err != nil || freshExecute == nil || freshExecute.IsError || freshExecute.StructuredContent == nil {
		t.Fatalf("wire crash-state fresh execute result=%#v err=%v", freshExecute, err)
	}
	encodedFreshExecute, _ := json.Marshal(freshExecute.StructuredContent)
	var executed ExecuteSyncJournalAliasRepairOutput
	if err := json.Unmarshal(encodedFreshExecute, &executed); err != nil {
		t.Fatal(err)
	}
	if executed.Requested != 1 || executed.Removed != 1 || len(executed.Items) != 1 || executed.Items[0].Status != "removed" || executed.ErrorCode != "" {
		t.Fatalf("wire crash-state fresh execute output=%#v", executed)
	}
}
