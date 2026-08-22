package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func trashedMCPPersistentUploadFixture(t *testing.T) (mcpPersistentUploadFixture, MCPSyncJournalTrashItem) {
	t.Helper()
	fixture := newMCPPersistentUploadFixture(t)
	preview := reviewedMCPJournalCleanup(t, fixture)
	result, output, err := fixture.ft.executeSyncJournalCleanup(context.Background(), nil, ExecuteSyncJournalCleanupArgs{
		OlderThanHours: 1, Limit: 10, ExpectCleanupID: preview.CleanupID,
	})
	if err != nil || result == nil || result.IsError || output.Trashed != 1 {
		t.Fatalf("trash fixture result=%#v output=%#v err=%v", result, output, err)
	}
	listed, err := listMCPSyncJournalTrash(fixture.store, ListSyncJournalTrashArgs{Limit: 10})
	if err != nil || listed.Returned != 1 || len(listed.Items) != 1 {
		t.Fatalf("trash fixture list=%#v err=%v", listed, err)
	}
	return fixture, listed.Items[0]
}

func TestListAndRestoreSyncJournalTrashRoundTripUsesReviewedIDs(t *testing.T) {
	fixture, item := trashedMCPPersistentUploadFixture(t)
	if item.PlanID != fixture.args.ExpectPlanID || item.RestoreID == "" || item.State != syncjournalpkg.StatusCompleted || item.TrashedAt == "" || item.TrashAgeMS < 0 || item.TrashRetentionMS != syncjournalpkg.DefaultTrashRetention.Milliseconds() || item.PurgeEligibleAt == "" || item.PurgeEligible {
		t.Fatalf("unexpected trash item=%#v", item)
	}
	encoded, _ := json.Marshal(item)
	for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("trash list leaked %q: %s", forbidden, encoded)
		}
	}

	result, restored, err := fixture.ft.restoreSyncJournal(context.Background(), nil, RestoreSyncJournalArgs{
		PlanID: item.PlanID, ExpectRestoreID: item.RestoreID,
	})
	if err != nil || result == nil || result.IsError || !restored.Restored || restored.ErrorCode != "" || restored.PlanID != fixture.args.ExpectPlanID || restored.State != syncjournalpkg.StatusCompleted {
		t.Fatalf("restore result=%#v output=%#v err=%v", result, restored, err)
	}
	if journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); err != nil || journal.State != syncjournalpkg.StatusCompleted {
		t.Fatalf("restored current journal=%#v err=%v", journal, err)
	}
	if resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("restored review alias resolved=%q err=%v", resolved, err)
	}
	listed, err := listMCPSyncJournalTrash(fixture.store, ListSyncJournalTrashArgs{Limit: 10})
	if err != nil || listed.Returned != 0 {
		t.Fatalf("restored trash entry remains listed: %#v err=%v", listed, err)
	}
}

func TestRestoreSyncJournalRejectsStaleTokenBeforeMutation(t *testing.T) {
	fixture, item := trashedMCPPersistentUploadFixture(t)
	wrong := "sha256:" + strings.Repeat("f", 64)
	if wrong == item.RestoreID {
		wrong = "sha256:" + strings.Repeat("e", 64)
	}
	result, output, err := fixture.ft.restoreSyncJournal(context.Background(), nil, RestoreSyncJournalArgs{PlanID: item.PlanID, ExpectRestoreID: wrong})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "restore_changed" || output.Restored {
		t.Fatalf("stale restore result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("stale restore created current journal: %v", err)
	}
	listed, err := listMCPSyncJournalTrash(fixture.store, ListSyncJournalTrashArgs{Limit: 10})
	if err != nil || listed.Returned != 1 || listed.Items[0].RestoreID != item.RestoreID {
		t.Fatalf("stale restore mutated trash: %#v err=%v", listed, err)
	}
}

func TestRestoreSyncJournalTokenBindsHiddenReviewAliasSidecar(t *testing.T) {
	fixture, item := trashedMCPPersistentUploadFixture(t)
	scan, err := fixture.store.ScanTrashedCurrent(16)
	if err != nil || len(scan.Records) != 1 || len(scan.Records[0].ReviewIDs) == 0 {
		t.Fatalf("sidecar-bound trash scan=%#v err=%v", scan, err)
	}
	record := scan.Records[0]
	trashPath := filepath.Join(fixture.store.Root, "trash", record.TrashName)
	otherReviewID := "sha256:" + strings.Repeat("a", 64)
	if otherReviewID == item.PlanID {
		otherReviewID = "sha256:" + strings.Repeat("b", 64)
	}
	aliases := append([]string(nil), record.ReviewIDs...)
	aliases = append(aliases, otherReviewID)
	if err := syncjournalpkg.WriteTrashReviewAliases(trashPath, aliases); err != nil {
		t.Fatal(err)
	}
	// Preserve the reviewed directory mtime to prove restore_id is bound to the
	// private sidecar contents themselves rather than relying on mtime drift.
	if err := os.Chtimes(trashPath, record.TrashedAt, record.TrashedAt); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.restoreSyncJournal(context.Background(), nil, RestoreSyncJournalArgs{PlanID: item.PlanID, ExpectRestoreID: item.RestoreID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "restore_changed" || output.Restored {
		t.Fatalf("sidecar-tampered restore result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("sidecar-tampered restore created current journal: %v", err)
	}
}

func TestRestoreSyncJournalRefusesCurrentCollisionAndPreservesTrash(t *testing.T) {
	fixture, item := trashedMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.restoreSyncJournal(context.Background(), nil, RestoreSyncJournalArgs{PlanID: item.PlanID, ExpectRestoreID: item.RestoreID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "current_exists" || output.Restored {
		t.Fatalf("current collision restore result=%#v output=%#v err=%v", result, output, err)
	}
	listed, err := listMCPSyncJournalTrash(fixture.store, ListSyncJournalTrashArgs{Limit: 10})
	if err != nil || listed.Returned != 1 {
		t.Fatalf("current collision consumed trash: %#v err=%v", listed, err)
	}
}

func TestSyncJournalTrashWireListAndRestoreStayCredentialFree(t *testing.T) {
	fixture, _ := trashedMCPPersistentUploadFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "sync-trash-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "sync-trash-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	listResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_sync_journal_trash", Arguments: map[string]any{"limit": 10}})
	if err != nil || listResult == nil || listResult.IsError || listResult.StructuredContent == nil || len(listResult.Content) != 1 {
		t.Fatalf("trash list wire result=%#v err=%v", listResult, err)
	}
	listEncoded, _ := json.Marshal(listResult.StructuredContent)
	var listed ListSyncJournalTrashOutput
	if err := json.Unmarshal(listEncoded, &listed); err != nil || len(listed.Items) != 1 {
		t.Fatalf("trash list wire decode=%#v err=%v", listed, err)
	}
	listText := listResult.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(listEncoded), listText} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("trash list wire leaked %q: %s", forbidden, payload)
			}
		}
	}

	restoreResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "restore_sync_journal", Arguments: map[string]any{"plan_id": listed.Items[0].PlanID, "expect_restore_id": listed.Items[0].RestoreID},
	})
	if err != nil || restoreResult == nil || restoreResult.IsError || restoreResult.StructuredContent == nil || len(restoreResult.Content) != 1 {
		t.Fatalf("trash restore wire result=%#v err=%v", restoreResult, err)
	}
	restoreEncoded, _ := json.Marshal(restoreResult.StructuredContent)
	restoreText := restoreResult.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(restoreEncoded), restoreText} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("trash restore wire leaked %q: %s", forbidden, payload)
			}
		}
		if strings.Contains(strings.ToLower(payload), "trash_path") || strings.Contains(strings.ToLower(payload), "journal_path") || strings.Contains(strings.ToLower(payload), "remote_id") || strings.Contains(strings.ToLower(payload), "sha1") || strings.Contains(strings.ToLower(payload), "postcondition") {
			t.Fatalf("trash restore wire leaked hidden journal evidence: %s", payload)
		}
	}
	if _, err := os.Stat(fixture.store.Root + string(os.PathSeparator) + "trash"); err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspect shared trash root after restore: %v", err)
	}
}
