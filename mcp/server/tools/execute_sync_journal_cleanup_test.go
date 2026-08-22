package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reviewedMCPJournalCleanup(t *testing.T, fixture mcpPersistentUploadFixture) PlanSyncJournalCleanupOutput {
	t.Helper()
	ageMCPCompletedJournalForCleanup(t, fixture, 48*time.Hour)
	result, output, err := fixture.ft.planSyncJournalCleanup(context.Background(), nil, PlanSyncJournalCleanupArgs{OlderThanHours: 1, Limit: 10})
	if err != nil || result == nil || result.IsError || output.Selected != 1 || len(output.Items) != 1 || output.CleanupID == "" {
		t.Fatalf("review cleanup result=%#v output=%#v err=%v", result, output, err)
	}
	return output
}

func TestExecuteSyncJournalCleanupMovesReviewedJournalToTrashAndRemovesAlias(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	preview := reviewedMCPJournalCleanup(t, fixture)
	result, output, err := fixture.ft.executeSyncJournalCleanup(context.Background(), nil, ExecuteSyncJournalCleanupArgs{
		OlderThanHours: 1, Limit: 10, ExpectCleanupID: preview.CleanupID,
	})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Requested != 1 || output.Trashed != 1 || output.Failed != 0 || output.Skipped != 0 || output.Partial || len(output.Items) != 1 || output.Items[0].PlanID != fixture.args.ExpectPlanID || output.Items[0].Status != "trashed" {
		t.Fatalf("execute cleanup result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("cleaned current journal survives: %v", err)
	}
	if _, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("cleaned journal review alias survives: %v", err)
	}
	entries, err := os.ReadDir(fixture.store.Root + string(os.PathSeparator) + "trash")
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("session trash entries=%v err=%v", entries, err)
	}
	encoded, _ := json.Marshal(output)
	payload := string(encoded)
	for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("cleanup execution leaked %q: %s", forbidden, payload)
		}
	}
}

func TestExecuteSyncJournalCleanupRejectsChangedReviewBeforeMutation(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	preview := reviewedMCPJournalCleanup(t, fixture)
	wrongID := "sha256:" + strings.Repeat("f", 64)
	if wrongID == preview.CleanupID {
		wrongID = "sha256:" + strings.Repeat("e", 64)
	}
	result, output, err := fixture.ft.executeSyncJournalCleanup(context.Background(), nil, ExecuteSyncJournalCleanupArgs{
		OlderThanHours: 1, Limit: 10, ExpectCleanupID: wrongID,
	})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "cleanup_changed" || output.CleanupID != wrongID || output.Trashed != 0 {
		t.Fatalf("mismatched cleanup result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); err != nil {
		t.Fatalf("mismatched cleanup mutated journal: %v", err)
	}
	if resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("mismatched cleanup mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestExecuteSyncJournalCleanupReplansAfterCandidateChanges(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	preview := reviewedMCPJournalCleanup(t, fixture)
	location, err := fixture.store.Location(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := fixture.store.ReadCurrent(location)
	if err != nil {
		t.Fatal(err)
	}
	journal.UpdatedAt = time.Now().UTC()
	journal.CompletedAt = &journal.UpdatedAt
	if _, err := fixture.store.WriteCurrent(location, journal); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.executeSyncJournalCleanup(context.Background(), nil, ExecuteSyncJournalCleanupArgs{
		OlderThanHours: 1, Limit: 10, ExpectCleanupID: preview.CleanupID,
	})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "cleanup_changed" || output.Trashed != 0 {
		t.Fatalf("changed candidate cleanup result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); err != nil {
		t.Fatalf("changed cleanup candidate was mutated: %v", err)
	}
}

func TestExecuteSyncJournalCleanupRespectsSharedGCLockAndBulkMigrationLock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lockPath  func(root string) string
		leasePath func(root string) string
	}{
		{
			name:      "gc-lock",
			lockPath:  func(root string) string { return filepath.Join(root, "gc.lock") },
			leasePath: func(string) string { return "" },
		},
		{
			name:      "bulk-migration-lock",
			lockPath:  func(root string) string { return filepath.Join(root, "migration", "migrate-all.lock") },
			leasePath: func(root string) string { return filepath.Join(root, "migration", "migrate-all-lease.json") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMCPPersistentUploadFixture(t)
			preview := reviewedMCPJournalCleanup(t, fixture)
			root, err := fixture.store.RootPath()
			if err != nil {
				t.Fatal(err)
			}
			lock, err := transfer.AcquireSessionLock(tc.lockPath(root), tc.leasePath(root))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()

			result, output, err := fixture.ft.executeSyncJournalCleanup(context.Background(), nil, ExecuteSyncJournalCleanupArgs{
				OlderThanHours: 1, Limit: 10, ExpectCleanupID: preview.CleanupID,
			})
			if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_cleanup_in_use" || output.Trashed != 0 || output.Requested != 0 {
				t.Fatalf("cleanup under %s result=%#v output=%#v err=%v", tc.name, result, output, err)
			}
			if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); err != nil {
				t.Fatalf("cleanup under %s mutated current journal: %v", tc.name, err)
			}
			if resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); err != nil || resolved != fixture.state.Plan.PlanID {
				t.Fatalf("cleanup under %s mutated review alias: resolved=%q err=%v", tc.name, resolved, err)
			}
		})
	}
}

func TestExecuteSyncJournalCleanupWireReturnsSafeTypedResult(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	preview := reviewedMCPJournalCleanup(t, fixture)
	server := mcp.NewServer(&mcp.Implementation{Name: "execute-cleanup-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "execute-cleanup-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute_sync_journal_cleanup",
		Arguments: map[string]any{"older_than_hours": 1, "limit": 10, "expect_cleanup_id": preview.CleanupID},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("execute cleanup wire result=%#v err=%v", result, err)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("execute cleanup wire leaked %q: %s", forbidden, payload)
			}
		}
		if strings.Contains(strings.ToLower(payload), "journal_path") || strings.Contains(strings.ToLower(payload), "remote_id") || strings.Contains(strings.ToLower(payload), "postcondition") || strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("execute cleanup wire leaked hidden journal evidence: %s", payload)
		}
	}
}
