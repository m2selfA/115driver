package syncjournal

import (
	"errors"
	"fmt"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

var ErrInvalidSchema = errors.New("sync execution journal schema is invalid or incomplete")

// ValidateStoredItem checks the state/phase/postcondition relationships for one
// persisted journal item against its immutable reviewed-plan item.
func ValidateStoredItem(index int, stored Item, item syncplanpkg.Item) error {
	if stored.State == "succeeded" && stored.Phase != "done" {
		return fmt.Errorf("%w: sync journal item %d succeeded with phase %q", ErrInvalidSchema, index, stored.Phase)
	}
	if stored.State == "succeeded" && item.Action != "skip" && stored.Post == nil {
		return fmt.Errorf("%w: sync journal item %d succeeded without a postcondition", ErrInvalidSchema, index)
	}
	if stored.State == "skipped" {
		if item.Action != "skip" || stored.Phase != "done" || stored.Post != nil {
			return fmt.Errorf("%w: sync journal item %d has inconsistent skipped state", ErrInvalidSchema, index)
		}
	} else if item.Action == "skip" {
		return fmt.Errorf("%w: sync journal skip item %d has state %q", ErrInvalidSchema, index, stored.State)
	}
	if stored.Phase == "done" && stored.State != "succeeded" && stored.State != "skipped" {
		return fmt.Errorf("%w: sync journal item %d has terminal phase done with state %q", ErrInvalidSchema, index, stored.State)
	}
	if stored.Post != nil {
		if stored.State != "succeeded" || stored.Phase != "done" {
			return fmt.Errorf("%w: sync journal item %d has postcondition before successful completion", ErrInvalidSchema, index)
		}
		post := stored.Post
		switch item.Action {
		case "upload", "replace-remote":
			if post.Side != "remote" || !post.Exists || post.Kind != item.Kind {
				return fmt.Errorf("%w: sync journal item %d has invalid remote postcondition", ErrInvalidSchema, index)
			}
		case "download", "replace-local":
			if post.Side != "local" || !post.Exists || post.Kind != item.Kind {
				return fmt.Errorf("%w: sync journal item %d has invalid local postcondition", ErrInvalidSchema, index)
			}
		case "delete-remote":
			if post.Side != "remote" || post.Exists {
				return fmt.Errorf("%w: sync journal item %d has invalid remote deletion postcondition", ErrInvalidSchema, index)
			}
		case "delete-local":
			if post.Side != "local" || post.Exists {
				return fmt.Errorf("%w: sync journal item %d has invalid local deletion postcondition", ErrInvalidSchema, index)
			}
		default:
			return fmt.Errorf("%w: sync journal item %d action %q cannot carry a postcondition", ErrInvalidSchema, index, item.Action)
		}
	}
	return nil
}

func ValidateRunStats(stats RunStats) error {
	if stats.Runs < 0 || stats.ResumeRuns < 0 || stats.InterruptedRuns < 0 || stats.LastDurationMillis < 0 || stats.TotalDurationMillis < 0 {
		return fmt.Errorf("%w: sync journal run statistics contain negative values", ErrInvalidSchema)
	}
	if stats.ResumeRuns > stats.Runs || stats.InterruptedRuns > stats.Runs {
		return fmt.Errorf("%w: sync journal run statistics exceed total runs", ErrInvalidSchema)
	}
	if stats.LastFinishedAt != nil && stats.LastStartedAt == nil {
		return fmt.Errorf("%w: sync journal run statistics have a finish time without a start time", ErrInvalidSchema)
	}
	if stats.LastStartedAt != nil && stats.LastFinishedAt != nil && stats.LastFinishedAt.Before(*stats.LastStartedAt) {
		return fmt.Errorf("%w: sync journal run finish time precedes start time", ErrInvalidSchema)
	}
	if stats.LastFinishedAt == nil && stats.LastDurationMillis != 0 {
		return fmt.Errorf("%w: unfinished sync journal run has a persisted duration", ErrInvalidSchema)
	}
	if stats.TotalDurationMillis < stats.LastDurationMillis {
		return fmt.Errorf("%w: sync journal total run duration is smaller than the last duration", ErrInvalidSchema)
	}
	return nil
}
