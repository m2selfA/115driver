package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeListSyncExecutionsArgs(t *testing.T) {
	limit, status, err := normalizeListSyncExecutionsArgs(ListSyncExecutionsArgs{})
	if err != nil || limit != defaultMCPListSyncExecutionsLimit || status != "" {
		t.Fatalf("default list sync executions args = %d/%q/%v", limit, status, err)
	}
	limit, status, err = normalizeListSyncExecutionsArgs(ListSyncExecutionsArgs{Limit: maxMCPListSyncExecutionsLimit, Status: " Recovery-Required "})
	if err != nil || limit != maxMCPListSyncExecutionsLimit || status != syncjournalpkg.StatusRecoveryRequired {
		t.Fatalf("bounded list sync executions args = %d/%q/%v", limit, status, err)
	}
	for _, args := range []ListSyncExecutionsArgs{{Limit: -1}, {Limit: maxMCPListSyncExecutionsLimit + 1}, {Status: "secret-invalid-status"}} {
		if _, _, err := normalizeListSyncExecutionsArgs(args); err == nil {
			t.Fatalf("invalid list sync executions args accepted: %#v", args)
		}
	}
}

func prepareListSyncExecutionsFixture(t *testing.T) (mcpPersistentUploadFixture, *syncjournalpkg.Handle) {
	t.Helper()
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	legacyLocation, err := fixture.store.Location(strings.Repeat("e", 64))
	if err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := transfer.WritePrivateFileAtomic(legacyLocation.JournalPath, []byte(`{"version":1}`)); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	return fixture, handle
}

func TestListSyncExecutionsUsesReviewedPlanIDAndSafeSummary(t *testing.T) {
	fixture, handle := prepareListSyncExecutionsFixture(t)
	defer handle.Close()
	result, output, err := fixture.ft.listSyncExecutions(context.Background(), nil, ListSyncExecutionsArgs{Limit: 10, Status: "active"})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Returned != 1 || output.MigrationRequired != 1 || len(output.Items) != 1 {
		t.Fatalf("list sync executions result=%#v output=%#v err=%v", result, output, err)
	}
	item := output.Items[0]
	if item.PlanID != fixture.args.ExpectPlanID || item.State != syncjournalpkg.StatusActive || item.Status != syncjournalpkg.StatusActive || !item.InUse || item.Total != 1 || item.Pending != 1 || item.Completed != 0 {
		t.Fatalf("unexpected safe execution item: %#v", item)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath, "remote-object-secret-42"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("list sync executions leaked %q: %s", forbidden, encoded)
		}
	}
	if strings.Contains(strings.ToLower(string(encoded)), "sha1") {
		t.Fatalf("list sync executions leaked hash field: %s", encoded)
	}
}

func TestListSyncExecutionsWireStructuredContentIsSafe(t *testing.T) {
	fixture, handle := prepareListSyncExecutionsFixture(t)
	defer handle.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "list-sync-executions-test", Version: "1"}, nil)
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
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_sync_executions", Arguments: map[string]any{"limit": 10}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire list sync executions result=%#v err=%v", result, err)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output ListSyncExecutionsOutput
	if err := json.Unmarshal(structured, &output); err != nil {
		t.Fatal(err)
	}
	if output.Returned != 1 || output.Items[0].PlanID != fixture.args.ExpectPlanID || !output.Items[0].InUse {
		t.Fatalf("wire list sync executions output=%#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath, "remote-object-secret-42"} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("wire list sync executions leaked %q: %s", forbidden, payload)
			}
		}
		if strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("wire list sync executions leaked hash field: %s", payload)
		}
	}
}
