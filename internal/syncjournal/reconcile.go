package syncjournal

import (
	"fmt"
	"time"
)

type DestructiveDecision string

const (
	DestructiveCompleted  DestructiveDecision = "completed"
	DestructiveRetryFull  DestructiveDecision = "retry-full"
	DestructiveWinnerOnly DestructiveDecision = "winner-only"
	DestructiveAmbiguous  DestructiveDecision = "ambiguous"
)

// ClassifyDestructiveEvidence turns already-observed target evidence into the
// journal's deterministic recovery decision. For a missing replacement target,
// callers must validate the still-available winner source before accepting the
// WinnerOnly decision; source validation can require local/remote I/O and stays
// outside this pure state transition.
func ClassifyDestructiveEvidence(action string, targetExists, winnerMatches, originalMatches bool) (DestructiveDecision, error) {
	switch action {
	case "delete-remote", "delete-local":
		if !targetExists {
			return DestructiveCompleted, nil
		}
		if originalMatches {
			return DestructiveRetryFull, nil
		}
		return DestructiveAmbiguous, nil
	case "replace-remote", "replace-local":
		if !targetExists {
			return DestructiveWinnerOnly, nil
		}
		if winnerMatches {
			return DestructiveCompleted, nil
		}
		if originalMatches {
			return DestructiveRetryFull, nil
		}
		return DestructiveAmbiguous, nil
	default:
		return "", fmt.Errorf("unsupported destructive sync action %q", action)
	}
}

// ApplyDestructiveDecision records one already-classified destructive recovery
// decision in a journal item. Evidence collection remains caller-owned; this
// function is the shared pure state transition used by CLI and MCP after that
// evidence has been reviewed.
func ApplyDestructiveDecision(journal *Journal, index int, decision DestructiveDecision, post *Postcondition, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	if index < 0 || index >= len(journal.Items) || index >= len(journal.Plan.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	planned := journal.Plan.Items[index]
	if !IsDestructivePlanItem(planned) {
		return fmt.Errorf("sync journal item %q is not destructive", planned.RelativePath)
	}
	now = normalizeExecutionTime(now)
	stored := &journal.Items[index]
	switch decision {
	case DestructiveCompleted:
		if post == nil {
			return fmt.Errorf("completed destructive journal item %q has no postcondition", planned.RelativePath)
		}
		return SucceedItem(journal, index, planned, post, now)
	case DestructiveRetryFull:
		stored.State = "pending"
		stored.Phase = PhasePending
		stored.Post = nil
		stored.LastError = ""
		stored.UpdatedAt = now
		return nil
	case DestructiveWinnerOnly:
		if planned.Action != "replace-remote" && planned.Action != "replace-local" {
			return fmt.Errorf("winner-only recovery is invalid for action %q", planned.Action)
		}
		stored.State = "pending"
		stored.Phase = PhaseLoserRemoved
		stored.Post = nil
		stored.LastError = ""
		stored.UpdatedAt = now
		return nil
	case DestructiveAmbiguous:
		return fmt.Errorf("destructive journal item %q remains ambiguous", planned.RelativePath)
	default:
		return fmt.Errorf("invalid destructive recovery decision %q", decision)
	}
}
