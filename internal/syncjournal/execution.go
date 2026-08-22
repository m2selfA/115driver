package syncjournal

import (
	"fmt"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"strings"
	"time"
)

func normalizeExecutionTime(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC()
}

// BeginItem applies the shared journal transition immediately before one plan
// item is scheduled. It performs no I/O; callers persist the mutated journal
// atomically through Handle.Mutate or their equivalent store transaction.
func BeginItem(journal *Journal, index int, scheduled syncplanpkg.Item, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	if index < 0 || index >= len(journal.Items) || index >= len(journal.Plan.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	stored := &journal.Items[index]
	planned := journal.Plan.Items[index]
	if syncplanpkg.PathKey(scheduled.RelativePath) != syncplanpkg.PathKey(planned.RelativePath) {
		return fmt.Errorf("sync journal item index %d does not match scheduled path %q", index, scheduled.RelativePath)
	}
	if stored.State == "succeeded" || stored.State == "skipped" {
		if scheduled.Action == "skip" {
			return nil
		}
		return fmt.Errorf("sync journal item %q is already completed but was scheduled again", planned.RelativePath)
	}
	now = normalizeExecutionTime(now)
	stored.State = "running"
	stored.Attempts++
	stored.LastError = ""
	stored.UpdatedAt = now
	journal.State = StatusActive
	journal.LastError = ""
	journal.CompletedAt = nil
	return nil
}

// SetItemPhase records a validated execution phase for one immutable plan item.
func SetItemPhase(journal *Journal, index int, phase string, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	if index < 0 || index >= len(journal.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	if !IsValidPhase(phase) || phase == "" {
		return fmt.Errorf("sync journal item %d has invalid execution phase %q", index, phase)
	}
	stored := &journal.Items[index]
	stored.Phase = phase
	stored.UpdatedAt = normalizeExecutionTime(now)
	return nil
}

// FailItem records one action failure. A crossed destructive phase remains in
// place so EffectiveStatus can surface reconcile-required until evidence has
// been reviewed. Recovery-required itself is a stronger explicit latch and is
// set separately through RequireRecovery.
func FailItem(journal *Journal, index int, errText string, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	if index < 0 || index >= len(journal.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	errText = strings.TrimSpace(errText)
	if errText == "" {
		errText = "sync action failed"
	}
	now = normalizeExecutionTime(now)
	stored := &journal.Items[index]
	stored.State = "failed"
	stored.LastError = errText
	stored.UpdatedAt = now
	journal.State = StatusFailed
	journal.LastError = errText
	journal.CompletedAt = nil
	return nil
}

// SucceedItem records a verified terminal postcondition. Non-skip actions must
// provide a postcondition because the persisted schema relies on it for safe
// resume/reconciliation.
func SucceedItem(journal *Journal, index int, scheduled syncplanpkg.Item, post *Postcondition, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	if index < 0 || index >= len(journal.Items) || index >= len(journal.Plan.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	planned := journal.Plan.Items[index]
	stored := &journal.Items[index]
	if syncplanpkg.PathKey(scheduled.RelativePath) != syncplanpkg.PathKey(planned.RelativePath) {
		return fmt.Errorf("sync journal item index %d does not match scheduled path %q", index, scheduled.RelativePath)
	}
	if scheduled.Action == "skip" {
		if post != nil {
			return fmt.Errorf("sync journal skip item %d cannot carry a postcondition", index)
		}
		if stored.State == "succeeded" || stored.State == "skipped" {
			return nil
		}
		if planned.Action != "skip" {
			return fmt.Errorf("sync journal unfinished item %q cannot be completed by a residual skip", planned.RelativePath)
		}
		stored.State = "skipped"
		stored.Post = nil
	} else {
		if stored.State == "succeeded" || stored.State == "skipped" {
			return fmt.Errorf("sync journal item %q is already completed but reported another success", planned.RelativePath)
		}
		if post == nil {
			return fmt.Errorf("sync journal item %d succeeded without a postcondition", index)
		}
		stored.State = "succeeded"
		stored.Post = post
	}
	now = normalizeExecutionTime(now)
	stored.Phase = PhaseDone
	stored.LastError = ""
	stored.UpdatedAt = now
	return ValidateStoredItem(index, *stored, planned)
}

// RequireRecovery explicitly latches an ambiguous destructive state. The
// existing item phase and failure detail remain available for later review.
func RequireRecovery(journal *Journal, message string, now time.Time) error {
	if journal == nil {
		return fmt.Errorf("sync journal is nil")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "destructive sync state requires recovery review"
	}
	journal.State = StatusRecoveryRequired
	journal.LastError = message
	journal.CompletedAt = nil
	journal.UpdatedAt = normalizeExecutionTime(now)
	return nil
}
