package syncjournal

import (
	"errors"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestValidateStoredItemContract(t *testing.T) {
	file := syncplanpkg.Item{Action: "upload", Kind: "file"}
	if err := ValidateStoredItem(0, Item{State: "succeeded", Phase: "done", Post: &Postcondition{Side: "remote", Exists: true, Kind: "file"}}, file); err != nil {
		t.Fatalf("valid upload completion rejected: %v", err)
	}
	for name, stored := range map[string]Item{
		"missing-postcondition":   {State: "succeeded", Phase: "done"},
		"premature-postcondition": {State: "running", Phase: "mutation-started", Post: &Postcondition{Side: "remote", Exists: true, Kind: "file"}},
		"wrong-post-side":         {State: "succeeded", Phase: "done", Post: &Postcondition{Side: "local", Exists: true, Kind: "file"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateStoredItem(2, stored, file)
			if !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("invalid stored item error = %v", err)
			}
		})
	}
	if err := ValidateStoredItem(1, Item{State: "skipped", Phase: "done"}, syncplanpkg.Item{Action: "skip", Kind: "file"}); err != nil {
		t.Fatalf("valid skip rejected: %v", err)
	}
}

func TestValidateRunStatsContract(t *testing.T) {
	start := time.Unix(10, 0).UTC()
	finish := start.Add(time.Second)
	valid := RunStats{Runs: 2, ResumeRuns: 1, InterruptedRuns: 1, LastStartedAt: &start, LastFinishedAt: &finish, LastDurationMillis: 1000, TotalDurationMillis: 2000}
	if err := ValidateRunStats(valid); err != nil {
		t.Fatalf("valid run stats rejected: %v", err)
	}
	invalid := valid
	invalid.ResumeRuns = 3
	err := ValidateRunStats(invalid)
	if !errors.Is(err, ErrInvalidSchema) || !strings.Contains(err.Error(), "exceed total runs") {
		t.Fatalf("invalid run stats error = %v", err)
	}
}
