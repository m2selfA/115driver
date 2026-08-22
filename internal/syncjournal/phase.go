package syncjournal

import (
	"fmt"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

const (
	PhasePending         = "pending"
	PhaseDone            = "done"
	PhaseMutationStarted = "mutation-started"
	PhaseMutationDone    = "mutation-done"
	PhaseWinnerStarted   = "winner-started"
	PhaseWinnerCreated   = "winner-created"
	PhaseRemoveStarted   = "remove-started"
	PhaseLoserRemoved    = "loser-removed"
	PhaseDeleteStarted   = "delete-started"
	PhaseDeleted         = "deleted"
)

type MutationStage string

const (
	MutationStageWrite  MutationStage = "write"
	MutationStageRemove MutationStage = "remove"
	MutationStageDelete MutationStage = "delete"
)

func IsValidPhase(phase string) bool {
	switch phase {
	case "", PhasePending, PhaseDone, PhaseMutationStarted, PhaseMutationDone, PhaseWinnerStarted, PhaseWinnerCreated, PhaseRemoveStarted, PhaseLoserRemoved, PhaseDeleteStarted, PhaseDeleted:
		return true
	default:
		return false
	}
}

// PhaseRequiresReconciliation reports whether an unfinished destructive item
// has crossed out of its pristine pending phase. PhaseDone deliberately still
// requires reconciliation when paired with a non-terminal item state because
// that combination is malformed/ambiguous and must fail closed.
func PhaseRequiresReconciliation(phase string) bool {
	return phase != "" && phase != PhasePending
}

// MutationPhases is the shared destructive/winner phase contract used by the
// CLI journal and non-persistent frontends. Write covers create/upload/download
// callbacks; when the action is a replacement it represents the winner phase
// after the loser has already been removed.
func MutationPhases(item syncplanpkg.Item, stage MutationStage) (before, after string, err error) {
	switch stage {
	case MutationStageWrite:
		switch item.Action {
		case "replace-remote", "replace-local":
			return PhaseWinnerStarted, PhaseWinnerCreated, nil
		case "upload", "download":
			return PhaseMutationStarted, PhaseMutationDone, nil
		default:
			return "", "", fmt.Errorf("sync journal write stage is invalid for action %q", item.Action)
		}
	case MutationStageRemove:
		if item.Action != "replace-remote" && item.Action != "replace-local" {
			return "", "", fmt.Errorf("sync journal remove stage is invalid for action %q", item.Action)
		}
		return PhaseRemoveStarted, PhaseLoserRemoved, nil
	case MutationStageDelete:
		if item.Action != "delete-remote" && item.Action != "delete-local" {
			return "", "", fmt.Errorf("sync journal delete stage is invalid for action %q", item.Action)
		}
		return PhaseDeleteStarted, PhaseDeleted, nil
	default:
		return "", "", fmt.Errorf("unknown sync journal mutation stage %q", stage)
	}
}

// MutationFailureRequiresRecovery is intentionally conservative for a
// stateless executor: once a destructive remove/delete stage has begun, or a
// replacement winner stage is running after its loser was removed, a failure
// cannot be blindly replayed without observing current state again.
func MutationFailureRequiresRecovery(item syncplanpkg.Item, stage MutationStage) bool {
	switch stage {
	case MutationStageWrite:
		return item.Action == "replace-remote" || item.Action == "replace-local"
	case MutationStageRemove:
		return item.Action == "replace-remote" || item.Action == "replace-local"
	case MutationStageDelete:
		return item.Action == "delete-remote" || item.Action == "delete-local"
	default:
		return false
	}
}
