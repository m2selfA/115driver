package syncjournal

import (
	"strings"
	"testing"
	"time"
)

func TestReviewAliasRepairIDBindsCompleteValidatedSnapshot(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	alias, err := store.WriteReviewAlias("sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	first, err := ReviewAliasRepairID(alias)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReviewAliasRepairID(alias)
	if err != nil || second != first {
		t.Fatalf("repair token is unstable: first=%q second=%q err=%v", first, second, err)
	}
	if normalized, err := NormalizeReviewID(first); err != nil || normalized != first {
		t.Fatalf("repair token is not canonical sha256 ID: token=%q normalized=%q err=%v", first, normalized, err)
	}

	changed := alias
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Nanosecond)
	changedToken, err := ReviewAliasRepairID(changed)
	if err != nil || changedToken == first {
		t.Fatalf("updated_at did not change repair token: token=%q err=%v", changedToken, err)
	}
	changed = alias
	changed.PlanID = strings.Repeat("d", 64)
	changedToken, err = ReviewAliasRepairID(changed)
	if err != nil || changedToken == first {
		t.Fatalf("hidden raw plan identity did not change repair token: token=%q err=%v", changedToken, err)
	}

	invalid := alias
	invalid.AccountID = 0
	if _, err := ReviewAliasRepairID(invalid); err == nil {
		t.Fatal("repair token accepted alias without account binding")
	}
}
