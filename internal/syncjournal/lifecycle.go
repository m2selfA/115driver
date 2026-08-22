package syncjournal

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func NormalizePlanID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", errors.New("sync journal plan ID is empty")
	}
	if len(value) != 64 {
		return "", fmt.Errorf("sync journal plan ID must be 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("sync journal plan ID must be hexadecimal: %w", err)
	}
	return value, nil
}

// New builds the initial persisted journal state for one immutable reviewed
// sync plan. Callers supply time so tests and future non-CLI frontends can use
// the same constructor without embedding a clock dependency in the schema.
func New(plan syncplanpkg.Plan, profileScope string, accountID int64, now time.Time) (Journal, error) {
	planID, err := NormalizePlanID(plan.PlanID)
	if err != nil {
		return Journal{}, err
	}
	if syncplanpkg.Fingerprint(plan) != planID {
		return Journal{}, errors.New("sync plan fingerprint does not match plan ID")
	}
	now = now.UTC()
	plan.PlanID = planID
	journal := Journal{
		Version: Version, Schema: SchemaID, PlanID: planID, ProfileScope: profileScope, AccountID: accountID,
		State: StatusActive, CreatedAt: now, UpdatedAt: now, Plan: plan,
		Items: make([]Item, len(plan.Items)),
	}
	for index, item := range plan.Items {
		state := "pending"
		phase := PhasePending
		if item.Action == "skip" {
			state = "skipped"
			phase = PhaseDone
		}
		journal.Items[index] = Item{
			Index: index, RelativePath: item.RelativePath, Action: item.Action, Kind: item.Kind,
			State: state, Phase: phase, LocalModTimeUnixNano: item.LocalModTimeUnixNano,
			RemoteModTimeUnixNano: item.RemoteModTimeUnixNano, UpdatedAt: now,
		}
	}
	journal.Status = EffectiveStatus(journal)
	return journal, nil
}

func ValidateJournalState(state string) error {
	switch state {
	case StatusActive, StatusFailed, StatusCompleted, StatusRecoveryRequired:
		return nil
	default:
		return fmt.Errorf("%w: invalid journal state %q", ErrInvalidSchema, state)
	}
}

// RestoreStoredItem validates the persisted item envelope against its reviewed
// plan item and restores the hidden modification-time snapshot fields needed by
// plan fingerprint verification. It performs no filesystem or remote I/O.
func RestoreStoredItem(index int, stored Item, item *syncplanpkg.Item) error {
	if item == nil {
		return fmt.Errorf("sync journal item %d has no stored plan item", index)
	}
	if stored.Index != index || stored.RelativePath != item.RelativePath || stored.Action != item.Action || stored.Kind != item.Kind {
		return fmt.Errorf("sync journal item %d does not match stored plan", index)
	}
	item.LocalModTimeUnixNano = stored.LocalModTimeUnixNano
	item.RemoteModTimeUnixNano = stored.RemoteModTimeUnixNano
	switch stored.State {
	case "pending", "running", "succeeded", "skipped", "failed", "blocked":
	default:
		return fmt.Errorf("sync journal item %d has invalid state %q", index, stored.State)
	}
	if !IsValidPhase(stored.Phase) {
		return fmt.Errorf("%w: sync journal item %d has invalid phase %q", ErrInvalidSchema, index, stored.Phase)
	}
	return ValidateStoredItem(index, stored, *item)
}
