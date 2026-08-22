package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func ageMCPCompletedJournalForCleanup(t *testing.T, fixture mcpPersistentUploadFixture, age time.Duration) {
	t.Helper()
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" {
		t.Fatalf("complete cleanup fixture result=%#v output=%#v err=%v", result, output, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	aged := time.Now().UTC().Add(-age)
	journal.UpdatedAt = aged
	journal.CompletedAt = &aged
	location, err := fixture.store.Location(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteCurrent(location, journal); err != nil {
		t.Fatal(err)
	}
}

func TestPlanSyncJournalCleanupUsesReviewedIDsAndStableEligibility(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	ageMCPCompletedJournalForCleanup(t, fixture, 48*time.Hour)
	fixture.store.Retention = 24 * time.Hour
	fixture.ft.syncJournalStore.Retention = 24 * time.Hour

	result, output, err := fixture.ft.planSyncJournalCleanup(context.Background(), nil, PlanSyncJournalCleanupArgs{Limit: 10})
	if err != nil || result == nil || result.IsError || output.CleanupID == "" || output.Eligible != 1 || output.Selected != 1 || len(output.Items) != 1 {
		t.Fatalf("cleanup plan result=%#v output=%#v err=%v", result, output, err)
	}
	if output.RetentionMillis != (24*time.Hour).Milliseconds() || output.Items[0].PlanID != fixture.args.ExpectPlanID || output.Items[0].State != syncjournalpkg.StatusCompleted {
		t.Fatalf("cleanup plan metadata=%#v", output)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(encoded)
	for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("cleanup plan leaked %q: %s", forbidden, payload)
		}
	}
	if strings.Contains(strings.ToLower(payload), "sha1") || strings.Contains(strings.ToLower(payload), "postcondition") || strings.Contains(strings.ToLower(payload), "remote_id") {
		t.Fatalf("cleanup plan leaked hidden journal evidence: %s", payload)
	}

	second, secondOutput, secondErr := fixture.ft.planSyncJournalCleanup(context.Background(), nil, PlanSyncJournalCleanupArgs{Limit: 10})
	if secondErr != nil || second == nil || second.IsError || secondOutput.CleanupID != output.CleanupID {
		t.Fatalf("stable cleanup plan changed: first=%#v second=%#v err=%v", output, secondOutput, secondErr)
	}
}

func TestPlanSyncJournalCleanupRefusesBulkMigrationMarker(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	ageMCPCompletedJournalForCleanup(t, fixture, 48*time.Hour)
	root, err := fixture.store.RootPath()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "migration", "migrate-all.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(`{"schema":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.planSyncJournalCleanup(context.Background(), nil, PlanSyncJournalCleanupArgs{OlderThanHours: 1})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_migration_in_progress" || output.CleanupID != "" || len(output.Items) != 0 {
		t.Fatalf("migration marker cleanup result=%#v output=%#v err=%v", result, output, err)
	}
}

func TestPlanSyncJournalCleanupWireReturnsTypedSafePreview(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	ageMCPCompletedJournalForCleanup(t, fixture, 48*time.Hour)
	server := mcp.NewServer(&mcp.Implementation{Name: "cleanup-preview-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "cleanup-preview-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "plan_sync_journal_cleanup", Arguments: map[string]any{"older_than_hours": 1, "limit": 10}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("cleanup preview wire result=%#v err=%v", result, err)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("cleanup preview wire leaked %q: %s", forbidden, payload)
			}
		}
	}
}
