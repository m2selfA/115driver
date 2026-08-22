package syncjournal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestJournalSchemaConstantsAndJSONContract(t *testing.T) {
	if Version != 2 || MinReadableVersion != 1 || LayoutVersion != "v1" || SchemaID != "115driver.sync-journal" {
		t.Fatalf("unexpected journal schema constants: version=%d min=%d layout=%q schema=%q", Version, MinReadableVersion, LayoutVersion, SchemaID)
	}
	now := time.Unix(123, 0).UTC()
	journal := Journal{
		Version: Version, Schema: SchemaID, PlanID: strings.Repeat("a", 64), ProfileScope: "scope", AccountID: 42,
		State: StatusActive, Status: StatusActive, CreatedAt: now, UpdatedAt: now,
		Plan:  syncplanpkg.Plan{PlanID: strings.Repeat("a", 64), Direction: syncplanpkg.DirectionUpload},
		Items: []Item{{Index: 0, RelativePath: "file.bin", Action: "upload", Kind: "file", State: "pending", Attempts: 0, UpdatedAt: now}},
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version":2`, `"schema":"115driver.sync-journal"`, `"plan_id"`, `"profile_scope"`, `"run_stats"`, `"plan"`, `"items"`, `"relative_path":"file.bin"`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("journal JSON lost %s: %s", key, encoded)
		}
	}
	var decoded Journal
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != Version || decoded.Plan.PlanID != journal.Plan.PlanID || len(decoded.Items) != 1 || decoded.Items[0].RelativePath != "file.bin" {
		t.Fatalf("journal round-trip changed schema model: %#v", decoded)
	}
}
