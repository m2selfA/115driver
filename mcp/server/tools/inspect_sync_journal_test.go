package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prepareInspectSyncJournalFixture(t *testing.T) (*FileTools, string, []string) {
	t.Helper()
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	const (
		secretRemoteID = "remote-object-secret-998877"
		secretSHA1     = "0123456789ABCDEF0123456789ABCDEF01234567"
	)
	// The immutable stored plan already contains an absolute local path, remote
	// path, and SHA1. Add more secret-looking identities only to mutable raw
	// error state; the safe projection must omit both classes.
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = syncjournalpkg.PhaseMutationStarted
		journal.Items[0].Attempts = 3
		journal.Items[0].LastError = "raw failure at " + fixture.localPath + " /remote/source.bin " + secretRemoteID + " " + secretSHA1
		journal.LastError = journal.Items[0].LastError
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	// Alias-first lookup must not scan or parse unrelated current journals.
	corruptID := strings.Repeat("e", 64)
	corruptLocation, err := fixture.store.Location(corruptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(corruptLocation.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptLocation.JournalPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture.ft, fixture.args.ExpectPlanID, []string{fixture.localPath, "/remote/source.bin", secretRemoteID, secretSHA1, "raw failure at"}
}

func TestInspectSyncJournalSafeProjectionAndWireStructuredContent(t *testing.T) {
	ft, reviewedPlanID, forbidden := prepareInspectSyncJournalFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "inspect-sync-journal-test", Version: "1"}, nil)
	ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "inspect-sync-journal-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "inspect_sync_journal", Arguments: map[string]any{"plan_id": reviewedPlanID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("inspect_sync_journal wire result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		for _, secret := range forbidden {
			if strings.Contains(payload, secret) {
				t.Fatalf("inspect_sync_journal leaked %q: %s", secret, payload)
			}
		}
	}
	var output MCPSyncJournalInspection
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Found || output.PlanID != reviewedPlanID || output.State != syncjournalpkg.StatusFailed || output.Failed != 1 || output.TotalAttempts != 3 || len(output.Items) != 1 || output.Items[0].RelativePath != "source.bin" || !output.Items[0].HasError {
		t.Fatalf("unexpected inspect_sync_journal output=%#v", output)
	}
}

func TestInspectSyncJournalFallsBackForPreAliasCurrentV2(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("pre-alias fixture unexpectedly has alias: %v", err)
	}
	result, output, err := fixture.ft.inspectSyncJournal(context.Background(), nil, InspectSyncJournalArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || result == nil || result.IsError || !output.Found || output.PlanID != fixture.args.ExpectPlanID || len(output.Items) != 1 {
		t.Fatalf("pre-alias fallback result=%#v output=%#v err=%v", result, output, err)
	}
}

func TestSyncJournalReadOnlyLookupIgnoresStaleAliasAndFindsPreAliasCurrentV2(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	staleRawPlanID := strings.Repeat("7", 64)
	if staleRawPlanID == fixture.state.Plan.PlanID {
		staleRawPlanID = strings.Repeat("8", 64)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, staleRawPlanID); err != nil {
		t.Fatal(err)
	}

	lookup := fixture.ft.lookupMCPSyncJournal(context.Background(), fixture.args.ExpectPlanID)
	if !lookup.found() || lookup.Record == nil || lookup.Record.Journal.PlanID != fixture.state.Plan.PlanID || lookup.ErrorCode != "" {
		t.Fatalf("stale-alias bounded lookup = %#v", lookup)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != staleRawPlanID {
		t.Fatalf("read-only lookup mutated stale alias: resolved=%q err=%v", resolved, err)
	}

	result, output, err := fixture.ft.inspectSyncJournal(context.Background(), nil, InspectSyncJournalArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || result == nil || result.IsError || !output.Found || output.PlanID != fixture.args.ExpectPlanID || len(output.Items) != 1 {
		t.Fatalf("stale-alias inspect fallback result=%#v output=%#v err=%v", result, output, err)
	}
	resolvedAfter, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolvedAfter != staleRawPlanID {
		t.Fatalf("read-only inspect mutated stale alias: resolved=%q err=%v", resolvedAfter, err)
	}
}

func TestInspectSyncJournalMissingAndInvalidIDsAreSanitized(t *testing.T) {
	store := syncjournalpkg.Store{Root: t.TempDir(), ProfileScope: strings.Repeat("c", 64), AccountID: 42}
	ft := NewFileTools(nil, WithLocalRoot(t.TempDir()), WithSyncJournalStore(&store))
	for name, tc := range map[string]struct {
		planID string
		code   string
	}{
		"invalid": {planID: "secret-not-a-plan-id", code: "invalid_plan_id"},
		"missing": {planID: "sha256:" + strings.Repeat("d", 64), code: "journal_not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			result, output, err := ft.inspectSyncJournal(context.Background(), nil, InspectSyncJournalArgs{PlanID: tc.planID})
			if err != nil || result == nil || !result.IsError || output.ErrorCode != tc.code {
				t.Fatalf("inspect %s result=%#v output=%#v err=%v", name, result, output, err)
			}
			encoded, _ := json.Marshal(output)
			if strings.Contains(string(encoded), "secret-not-a-plan-id") {
				t.Fatalf("invalid plan id reflected in output: %s", encoded)
			}
		})
	}
}
