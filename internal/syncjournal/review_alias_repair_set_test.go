package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReviewAliasRepairSetIDBindsCompleteOrphanSetAndLimit(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("1", 64), AccountID: 42}
	first, err := store.WriteReviewAlias("sha256:"+strings.Repeat("2", 64), strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.WriteReviewAlias("sha256:"+strings.Repeat("4", 64), strings.Repeat("5", 64))
	if err != nil {
		t.Fatal(err)
	}
	base, err := ReviewAliasRepairSetID(1, []ReviewAlias{second, first})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := ReviewAliasRepairSetID(1, []ReviewAlias{first, second})
	if err != nil || reordered != base {
		t.Fatalf("repair set ordering changed token: base=%q reordered=%q err=%v", base, reordered, err)
	}
	changed := second
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Nanosecond)
	changedToken, err := ReviewAliasRepairSetID(1, []ReviewAlias{first, changed})
	if err != nil || changedToken == base {
		t.Fatalf("unselected orphan change did not invalidate token: token=%q err=%v", changedToken, err)
	}
	limitToken, err := ReviewAliasRepairSetID(2, []ReviewAlias{first, second})
	if err != nil || limitToken == base {
		t.Fatalf("limit change did not invalidate token: token=%q err=%v", limitToken, err)
	}
	if _, err := ReviewAliasRepairSetID(1, []ReviewAlias{first, first}); err == nil {
		t.Fatal("duplicate repair-set alias was accepted")
	}
}

func TestBuildReviewAliasRepairPlanSelectsSortedOrphansAndRejectsAggregateDrift(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("6", 64), AccountID: 42}
	first, err := store.WriteReviewAlias("sha256:"+strings.Repeat("2", 64), strings.Repeat("7", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.WriteReviewAlias("sha256:"+strings.Repeat("4", 64), strings.Repeat("8", 64))
	if err != nil {
		t.Fatal(err)
	}
	scan := ReviewAliasDiagnosisScan{
		Scanned: 3,
		Live:    1,
		Orphan:  2,
		Issues:  2,
		Entries: []ReviewAliasDiagnosis{
			{Alias: second, Status: ReviewAliasDiagnosisOrphan},
			{Alias: first, Status: ReviewAliasDiagnosisOrphan},
			{Status: ReviewAliasDiagnosisLive},
		},
	}
	plan, err := BuildReviewAliasRepairPlan(scan, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantSetID, err := ReviewAliasRepairSetID(1, []ReviewAlias{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if plan.RepairSetID != wantSetID || plan.Scanned != 3 || plan.Eligible != 2 || plan.Limit != 1 || len(plan.Candidates) != 1 {
		t.Fatalf("shared repair plan=%#v want set=%q", plan, wantSetID)
	}
	if plan.Candidates[0].Alias.ReviewID != first.ReviewID {
		t.Fatalf("shared repair plan did not select sorted prefix: %#v", plan.Candidates)
	}
	wantRepairID, err := ReviewAliasRepairID(first)
	if err != nil || plan.Candidates[0].RepairID != wantRepairID {
		t.Fatalf("shared repair candidate token=%q want=%q err=%v", plan.Candidates[0].RepairID, wantRepairID, err)
	}
	inconsistent := scan
	inconsistent.Orphan = 3
	if _, err := BuildReviewAliasRepairPlan(inconsistent, 1); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("aggregate-drift repair plan error=%v, want ErrInvalidSchema", err)
	}
	scannedDrift := scan
	scannedDrift.Scanned = 4
	if _, err := BuildReviewAliasRepairPlan(scannedDrift, 1); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("scanned-drift repair plan error=%v, want ErrInvalidSchema", err)
	}
	unknown := scan
	unknown.Live = 0
	unknown.Issues = 3
	unknown.Entries[2].Status = ReviewAliasDiagnosisStatus("future-unknown")
	if _, err := BuildReviewAliasRepairPlan(unknown, 1); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("unknown-status repair plan error=%v, want ErrInvalidSchema", err)
	}
}
