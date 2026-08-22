package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestValidateRecyclePaginationDefaultsAndRejectsInvalidValues(t *testing.T) {
	offset, limit, err := validateRecyclePagination(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 || limit != defaultRecycleLimit {
		t.Fatalf("unexpected default pagination: offset=%d limit=%d", offset, limit)
	}

	for name, values := range map[string][2]int{
		"negative-offset": {-1, 0},
		"negative-limit":  {0, -1},
		"oversized-limit": {0, maxRecycleLimit + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := validateRecyclePagination(values[0], values[1]); err == nil {
				t.Fatal("expected invalid pagination error")
			}
		})
	}
}

func TestRecycleMutationDryRunsValidateWithoutClientOrPasswordEcho(t *testing.T) {
	rt := NewRecycleTools(nil, WithRecycleDestructiveTools(true))

	revert, _, err := rt.revertRecycleBin(context.Background(), nil, RevertRecycleArgs{ItemIDs: []string{" 1 ", "2"}, DryRun: true})
	if err != nil || revert == nil || revert.IsError || len(revert.Content) != 1 {
		t.Fatalf("revert dry-run = %#v, %v", revert, err)
	}

	const password = "super-secret-recycle-password"
	clean, _, err := rt.cleanRecycleBin(context.Background(), nil, CleanRecycleArgs{Password: password, ItemIDs: []string{"3"}, DryRun: true})
	if err != nil || clean == nil || clean.IsError || len(clean.Content) != 1 {
		t.Fatalf("clean dry-run = %#v, %v", clean, err)
	}
	text, ok := clean.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected clean dry-run content: %#v", clean.Content[0])
	}
	if strings.Contains(text.Text, password) {
		t.Fatalf("clean dry-run echoed recycle password: %s", text.Text)
	}
}

func TestCleanRecycleBinWireDryRunPopulatesSafeStructuredContent(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "recycle-typed-test", Version: "1"}, nil)
	NewRecycleTools(nil, WithRecycleDestructiveTools(true)).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "recycle-typed-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	const password = "wire-secret-recycle-password"
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "cleanRecycleBin",
		Arguments: map[string]any{
			"password": password,
			"item_ids": []any{"item-1", "item-2"},
			"dry_run":  true,
		},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire recycle dry-run result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), password) || strings.Contains(strings.ToLower(string(encoded)), "password") {
		t.Fatalf("wire recycle structured output leaked password: %s", encoded)
	}
	var output MCPRecycleMutationOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Mode != "dry_run" || output.Plan == nil || output.Result != nil || output.Plan.Operation != "clean_recycle" || output.Plan.Requested != 2 {
		t.Fatalf("unexpected recycle structured dry-run: %#v", output)
	}
}

func TestRecycleMutationPreflightRejectsDuplicatesAndOversizeBeforeClient(t *testing.T) {
	rt := NewRecycleTools(nil, WithRecycleDestructiveTools(true))
	if result, _, err := rt.revertRecycleBin(context.Background(), nil, RevertRecycleArgs{ItemIDs: []string{"1", " 1 "}}); err != nil || result == nil || !result.IsError {
		t.Fatalf("duplicate recycle ids = %#v, %v", result, err)
	}
	if result, _, err := rt.cleanRecycleBin(context.Background(), nil, CleanRecycleArgs{ItemIDs: make([]string, maxMCPMutationBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized recycle ids = %#v, %v", result, err)
	}
}
