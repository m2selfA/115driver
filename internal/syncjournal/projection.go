package syncjournal

import (
	"fmt"
	"strings"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

// ApplyPostcondition projects a verified journal postcondition onto one stored
// sync-plan item. It performs no filesystem or remote I/O.
func ApplyPostcondition(item *syncplanpkg.Item, post *Postcondition) {
	if item == nil || post == nil {
		return
	}
	switch post.Side {
	case "remote":
		item.RemotePresent = post.Exists
		item.RemoteID = post.RemoteID
		item.RemoteSize = post.Size
		item.RemoteSHA1 = strings.ToUpper(strings.TrimSpace(post.SHA1))
		item.RemoteModTimeUnixNano = post.ModTimeUnixNano
		if post.Exists && post.Kind != "" {
			item.Kind = post.Kind
		}
	case "local":
		item.LocalPresent = post.Exists
		item.LocalSize = post.Size
		item.LocalSHA1 = strings.ToUpper(strings.TrimSpace(post.SHA1))
		item.LocalModTimeUnixNano = post.ModTimeUnixNano
		if post.Exists && post.Kind != "" {
			item.Kind = post.Kind
		}
	}
}

// ClearPlannedLoserSide removes the planned snapshot for the side that a
// destructive delete/replacement action removes. It is used only after journal
// evidence proves that loser side is absent.
func ClearPlannedLoserSide(item *syncplanpkg.Item, action string) {
	if item == nil {
		return
	}
	switch action {
	case "delete-remote", "replace-remote":
		item.RemotePresent = false
		item.RemoteID = ""
		item.RemoteSize = 0
		item.RemoteSHA1 = ""
		item.RemoteModTimeUnixNano = 0
	case "delete-local", "replace-local":
		item.LocalPresent = false
		item.LocalSize = 0
		item.LocalSHA1 = ""
		item.LocalModTimeUnixNano = 0
	}
}

// ExpectedPlan projects persisted journal evidence into the exact tree state a
// resume preflight must observe before any residual action may run. Completed
// items are converted to skips with their verified postconditions applied.
func ExpectedPlan(journal Journal) (syncplanpkg.Plan, error) {
	if len(journal.Items) != len(journal.Plan.Items) {
		return syncplanpkg.Plan{}, fmt.Errorf("%w: sync journal item count does not match stored plan", ErrInvalidSchema)
	}
	plan := journal.Plan
	plan.Items = append([]syncplanpkg.Item(nil), journal.Plan.Items...)
	removedRoots := make(map[string]string)
	for index, stored := range journal.Items {
		original := journal.Plan.Items[index]
		if stored.State == "succeeded" && IsDestructivePlanItem(original) {
			removedRoots[syncplanpkg.PathKey(original.RelativePath)] = original.Action
			continue
		}
		if stored.State != "succeeded" && stored.Phase == PhaseLoserRemoved && (original.Action == "replace-remote" || original.Action == "replace-local") {
			removedRoots[syncplanpkg.PathKey(original.RelativePath)] = original.Action
		}
	}
	for index := range plan.Items {
		item := &plan.Items[index]
		stored := journal.Items[index]
		if stored.State == "succeeded" {
			if stored.Post == nil {
				return syncplanpkg.Plan{}, fmt.Errorf("completed sync journal item %q has no postcondition", item.RelativePath)
			}
			ApplyPostcondition(item, stored.Post)
			item.Action = "skip"
			item.Reason = "journal-completed:" + stored.Action
			continue
		}
		if stored.Phase == PhaseLoserRemoved && (item.Action == "replace-remote" || item.Action == "replace-local") {
			ClearPlannedLoserSide(item, item.Action)
		}
	}
	for index := range plan.Items {
		item := &plan.Items[index]
		if item.Action != "skip" {
			continue
		}
		for root, action := range removedRoots {
			if strings.HasPrefix(syncplanpkg.PathKey(item.RelativePath), root+"/") {
				ClearPlannedLoserSide(item, action)
			}
		}
	}
	return plan, nil
}

// ResidualPlan converts a reconciled journal into the plan that remains to be
// executed. Completed/skipped items stay as skips. A replacement whose loser is
// already proven removed resumes only its non-destructive winner phase.
func ResidualPlan(journal Journal) syncplanpkg.Plan {
	plan := journal.Plan
	plan.Items = append([]syncplanpkg.Item(nil), journal.Plan.Items...)
	plan.DestructiveActions = 0
	for index := range plan.Items {
		stored := journal.Items[index]
		item := &plan.Items[index]
		if stored.State == "succeeded" || stored.State == "skipped" {
			item.Action = "skip"
			item.Reason = "journal-completed:" + stored.Action
			continue
		}
		if stored.Phase == PhaseLoserRemoved {
			switch item.Action {
			case "replace-remote":
				item.Action = "upload"
				item.Reason = "journal-resume-winner-only:replace-remote"
				item.Destructive = false
				item.ReplacesKind = ""
			case "replace-local":
				item.Action = "download"
				item.Reason = "journal-resume-winner-only:replace-local"
				item.Destructive = false
				item.ReplacesKind = ""
			}
		}
		if IsDestructivePlanItem(*item) {
			plan.DestructiveActions++
		}
	}
	plan.ChangeActions = syncplanpkg.ChangeCount(plan)
	plan.RequiresAllowDestructive = plan.DestructiveActions > 0
	return plan
}
