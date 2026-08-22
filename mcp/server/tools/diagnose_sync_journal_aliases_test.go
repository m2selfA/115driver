package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prepareSyncJournalAliasDiagnosisFixture(t *testing.T) (mcpPersistentUploadFixture, map[string]string, []string) {
	t.Helper()
	fixture := newMCPPersistentUploadFixture(t)
	liveHandle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := liveHandle.BindReviewAlias(fixture.args.ExpectPlanID); err != nil {
		liveHandle.Close()
		t.Fatal(err)
	}
	if err := liveHandle.Close(); err != nil {
		t.Fatal(err)
	}

	identityMismatch := "sha256:" + strings.Repeat("f", 64)
	if identityMismatch == fixture.args.ExpectPlanID {
		identityMismatch = "sha256:" + strings.Repeat("e", 64)
	}
	if _, err := fixture.store.WriteReviewAlias(identityMismatch, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}

	orphanReview := "sha256:" + strings.Repeat("d", 64)
	orphanRaw := strings.Repeat("8", 64)
	if _, err := fixture.store.WriteReviewAlias(orphanReview, orphanRaw); err != nil {
		t.Fatal(err)
	}

	softPlan := fixture.state.Plan
	softPlan.RemoteRootID = "soft-deleted-root-id"
	softPlan.PlanID = ""
	softPlan.PlanID = syncplanpkg.Fingerprint(softPlan)
	softEnvelope, err := buildMCPSyncPlanEnvelope(softPlan)
	if err != nil {
		t.Fatal(err)
	}
	softHandle, err := fixture.store.CreateCurrent(softPlan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := softHandle.BindReviewAlias(softEnvelope.PlanID); err != nil {
		softHandle.Close()
		t.Fatal(err)
	}
	if err := softHandle.Close(); err != nil {
		t.Fatal(err)
	}
	softLocation, err := fixture.store.Location(softPlan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncjournalpkg.MoveDirectoryToSessionTrash(fixture.store.Root, softLocation.Dir, softPlan.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		fixture.args.ExpectPlanID: "live",
		identityMismatch:          "identity-mismatch",
		orphanReview:              "orphan",
		softEnvelope.PlanID:       "soft-deleted-shadow",
	}
	forbidden := []string{fixture.state.Plan.PlanID, softPlan.PlanID, fixture.store.Root, fixture.localPath, "soft-deleted-root-id"}
	return fixture, want, forbidden
}

func assertSyncJournalAliasDiagnosis(t *testing.T, output DiagnoseSyncJournalAliasesOutput, want map[string]string) {
	t.Helper()
	if output.Scanned != 4 || output.Returned != 4 || output.Live != 1 || output.Orphan != 1 || output.SoftDeleted != 1 || output.IdentityMismatch != 1 || output.Invalid != 0 || output.Issues != 3 {
		t.Fatalf("alias diagnosis aggregate=%#v", output)
	}
	got := make(map[string]string, len(output.Items))
	for _, item := range output.Items {
		got[item.PlanID] = item.Status
		if item.Status == string(syncjournalpkg.ReviewAliasDiagnosisOrphan) {
			if normalized, err := normalizeMCPExpectedPlanID(item.RepairID); err != nil || normalized == "" || normalized != item.RepairID {
				t.Fatalf("orphan repair_id=%q err=%v", item.RepairID, err)
			}
		} else if item.RepairID != "" {
			t.Fatalf("non-orphan alias unexpectedly received repair_id: %#v", item)
		}
	}
	for planID, status := range want {
		if got[planID] != status {
			t.Fatalf("alias %s status=%q want=%q output=%#v", planID, got[planID], status, output)
		}
	}
}

func TestDiagnoseSyncJournalAliasesClassifiesLifecycleWithoutMutation(t *testing.T) {
	fixture, want, forbidden := prepareSyncJournalAliasDiagnosisFixture(t)
	result, output, err := fixture.ft.diagnoseSyncJournalAliases(context.Background(), nil, DiagnoseSyncJournalAliasesArgs{Limit: 10})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" {
		t.Fatalf("alias diagnosis result=%#v output=%#v err=%v", result, output, err)
	}
	assertSyncJournalAliasDiagnosis(t, output, want)
	encoded, _ := json.Marshal(output)
	for _, secret := range forbidden {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("alias diagnosis leaked %q: %s", secret, encoded)
		}
	}
	for planID := range want {
		if _, err := fixture.store.ResolveReviewAlias(planID); err != nil {
			t.Fatalf("read-only alias diagnosis mutated %s: %v", planID, err)
		}
	}
}

func TestDiagnoseSyncJournalAliasesWireStructuredContentStaysSafe(t *testing.T) {
	fixture, want, forbidden := prepareSyncJournalAliasDiagnosisFixture(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "alias-diagnosis-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "alias-diagnosis-client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "diagnose_sync_journal_aliases", Arguments: map[string]any{"limit": 10}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire alias diagnosis result=%#v err=%v", result, err)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output DiagnoseSyncJournalAliasesOutput
	if err := json.Unmarshal(structured, &output); err != nil {
		t.Fatal(err)
	}
	assertSyncJournalAliasDiagnosis(t, output, want)
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, secret := range forbidden {
			if strings.Contains(payload, secret) {
				t.Fatalf("wire alias diagnosis leaked %q: %s", secret, payload)
			}
		}
		lower := strings.ToLower(payload)
		for _, hiddenField := range []string{"raw_plan_id", "internal_plan_id", "journal_path", "trash_path", "account_id", "profile_scope", "remote_id", "sha1", "postcondition"} {
			if strings.Contains(lower, hiddenField) {
				t.Fatalf("wire alias diagnosis leaked hidden field %q: %s", hiddenField, payload)
			}
		}
	}
}
