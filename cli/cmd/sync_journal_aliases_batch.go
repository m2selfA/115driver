package cmd

import (
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/spf13/cobra"
)

const (
	defaultCLISyncJournalAliasRepairLimit = syncjournalpkg.DefaultReviewAliasRepairBatchLimit
	maxCLISyncJournalAliasRepairLimit     = syncjournalpkg.MaxReviewAliasRepairBatchLimit
)

var (
	syncJournalAliasRepairPlanLimit   int
	syncJournalAliasRepairBatchLimit  int
	syncJournalAliasExpectRepairSetID string
)

type syncJournalAliasRepairBatchCandidate struct {
	ReviewID string `json:"review_id"`
	RepairID string `json:"repair_id"`
}

type syncJournalAliasRepairBatchPlan struct {
	RepairSetID string                                 `json:"repair_set_id"`
	Scanned     int                                    `json:"scanned"`
	Eligible    int                                    `json:"eligible"`
	Selected    int                                    `json:"selected"`
	Entries     []syncJournalAliasRepairBatchCandidate `json:"entries"`
}

type syncJournalAliasRepairBatchItem struct {
	Index    int    `json:"index"`
	ReviewID string `json:"review_id"`
	Status   string `json:"status"`
}

type syncJournalAliasRepairBatchResult struct {
	RepairSetID      string                            `json:"repair_set_id"`
	Requested        int                               `json:"requested"`
	Removed          int                               `json:"removed"`
	Unchanged        int                               `json:"unchanged"`
	Unknown          int                               `json:"unknown"`
	Partial          bool                              `json:"partial"`
	RecoveryRequired bool                              `json:"recovery_required,omitempty"`
	Items            []syncJournalAliasRepairBatchItem `json:"items"`
}

func normalizeCLISyncJournalAliasRepairLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("--limit must be >= 0")
	}
	if limit == 0 {
		return defaultCLISyncJournalAliasRepairLimit, nil
	}
	if limit > maxCLISyncJournalAliasRepairLimit {
		return 0, fmt.Errorf("--limit must not exceed %d", maxCLISyncJournalAliasRepairLimit)
	}
	return limit, nil
}

func planCLISyncJournalAliasRepair(store syncjournalpkg.Store, limit int) (syncJournalAliasRepairBatchPlan, []syncjournalpkg.ReviewAlias, error) {
	limit, err := normalizeCLISyncJournalAliasRepairLimit(limit)
	if err != nil {
		return syncJournalAliasRepairBatchPlan{}, nil, err
	}
	scan, err := store.DiagnoseReviewAliasesProfile(maxCLISyncJournalAliasScan, maxCLISyncJournalAliasScan, nil)
	if err != nil {
		return syncJournalAliasRepairBatchPlan{}, nil, err
	}
	sharedPlan, err := syncjournalpkg.BuildReviewAliasRepairPlan(scan, limit)
	if err != nil {
		return syncJournalAliasRepairBatchPlan{}, nil, err
	}
	selected := make([]syncjournalpkg.ReviewAlias, 0, len(sharedPlan.Candidates))
	plan := syncJournalAliasRepairBatchPlan{
		RepairSetID: sharedPlan.RepairSetID, Scanned: sharedPlan.Scanned, Eligible: sharedPlan.Eligible, Selected: len(sharedPlan.Candidates),
		Entries: make([]syncJournalAliasRepairBatchCandidate, 0, len(sharedPlan.Candidates)),
	}
	for _, candidate := range sharedPlan.Candidates {
		selected = append(selected, candidate.Alias)
		plan.Entries = append(plan.Entries, syncJournalAliasRepairBatchCandidate{ReviewID: candidate.Alias.ReviewID, RepairID: candidate.RepairID})
	}
	return plan, selected, nil
}

func cliAliasRepairBatchItems(selected []syncjournalpkg.ReviewAlias, status string) []syncJournalAliasRepairBatchItem {
	items := make([]syncJournalAliasRepairBatchItem, len(selected))
	for index, alias := range selected {
		items[index] = syncJournalAliasRepairBatchItem{Index: index, ReviewID: alias.ReviewID, Status: status}
	}
	return items
}

func cliAliasRepairBatchFailure(expectRepairSetID string, selected []syncjournalpkg.ReviewAlias, shared syncjournalpkg.ReviewAliasBatchRemovalResult) syncJournalAliasRepairBatchResult {
	status := "unchanged"
	result := syncJournalAliasRepairBatchResult{
		RepairSetID: expectRepairSetID, Requested: len(selected), Removed: shared.Removed,
		RecoveryRequired: shared.RecoveryRequired,
	}
	if shared.RecoveryRequired {
		status = "unknown"
		result.Unknown = len(selected)
		result.Partial = shared.Removed > 0
	} else {
		result.Unchanged = len(selected)
	}
	result.Items = cliAliasRepairBatchItems(selected, status)
	return result
}

func reconcileCLISyncJournalAliasBatch(store syncjournalpkg.Store, limit int, expectRepairSetID string) (syncJournalAliasRepairBatchResult, error) {
	limit, err := normalizeCLISyncJournalAliasRepairLimit(limit)
	if err != nil {
		return syncJournalAliasRepairBatchResult{}, err
	}
	expectRepairSetID, err = syncjournalpkg.NormalizeReviewID(expectRepairSetID)
	if err != nil {
		return syncJournalAliasRepairBatchResult{}, fmt.Errorf("invalid repair set ID: %w", err)
	}
	plan, selected, err := planCLISyncJournalAliasRepair(store, limit)
	if err != nil {
		return syncJournalAliasRepairBatchResult{}, err
	}
	if !sameCLISyncJournalAliasRepairToken(plan.RepairSetID, expectRepairSetID) {
		// Changed repair state must be explicitly replanned/reviewed. Do not
		// return the fresh selected prefix, its count, or the replacement token.
		return syncJournalAliasRepairBatchResult{RepairSetID: expectRepairSetID, Items: []syncJournalAliasRepairBatchItem{}}, errSyncJournalAliasRepairChanged
	}
	shared, removeErr := store.RemoveOrphanReviewAliasesExact(selected)
	if removeErr != nil {
		return cliAliasRepairBatchFailure(expectRepairSetID, selected, shared), removeErr
	}
	return syncJournalAliasRepairBatchResult{
		RepairSetID: expectRepairSetID, Requested: len(selected), Removed: shared.Removed,
		Items: cliAliasRepairBatchItems(selected, "removed"),
	}, nil
}

func syncJournalAliasesPlanArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, err := normalizeCLISyncJournalAliasRepairLimit(syncJournalAliasRepairPlanLimit); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return nil
}

var syncJournalAliasesPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Preview a reviewed bounded orphan-alias repair batch",
	Long:  "Preview a bounded orphan-only alias repair batch. repair_set_id binds the requested limit and the complete current orphan set, including unselected orphans. This is offline profile administration and may span persisted account bindings within the selected profile.",
	Args:  syncJournalAliasesPlanArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		plan, _, err := planCLISyncJournalAliasRepair(store.sharedCurrentStore(), syncJournalAliasRepairPlanLimit)
		if err != nil {
			return syncJournalExitError(err)
		}
		printer.PrintSuccess(plan)
		if !jsonOutput {
			fmt.Printf("Sync journal alias repair plan: scanned=%d eligible=%d selected=%d repair_set_id=%s\n", plan.Scanned, plan.Eligible, plan.Selected, plan.RepairSetID)
			for _, entry := range plan.Entries {
				fmt.Printf("%s  repair_id=%s\n", entry.ReviewID, entry.RepairID)
			}
		}
		return nil
	},
}

func syncJournalAliasesReconcileBatchArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, err := normalizeCLISyncJournalAliasRepairLimit(syncJournalAliasRepairBatchLimit); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if strings.TrimSpace(syncJournalAliasExpectRepairSetID) == "" {
		return &exitError{code: output.ExitArgs, msg: "--expect-repair-set-id is required"}
	}
	if _, err := syncjournalpkg.NormalizeReviewID(syncJournalAliasExpectRepairSetID); err != nil {
		return &exitError{code: output.ExitArgs, msg: "invalid --expect-repair-set-id: " + err.Error()}
	}
	return nil
}

var syncJournalAliasesReconcileBatchCmd = &cobra.Command{
	Use:   "reconcile-batch",
	Short: "Remove an exactly reviewed bounded batch of orphan aliases",
	Long:  "Remove only the orphan batch authorized by the exact repair_set_id from 'sync journal aliases plan'. Changed or stale state removes nothing and requires a fresh plan. Catchable removal failures roll back while the full lock set is held. Abrupt process termination is not power-loss atomic: aliases already removed may stay absent, but a fresh diagnosis/plan monotonically converges on the remaining orphan set and invalidates the old token before further removal. This command is offline/profile-scoped and may span persisted account bindings in that profile.",
	Args:  syncJournalAliasesReconcileBatchArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		result, err := reconcileCLISyncJournalAliasBatch(store.sharedCurrentStore(), syncJournalAliasRepairBatchLimit, syncJournalAliasExpectRepairSetID)
		if err != nil {
			return syncJournalExitErrorData(err, result)
		}
		printer.PrintSuccess(result)
		if !jsonOutput {
			fmt.Printf("Sync journal alias batch repair: requested=%d removed=%d unchanged=%d unknown=%d partial=%t recovery-required=%t repair_set_id=%s\n", result.Requested, result.Removed, result.Unchanged, result.Unknown, result.Partial, result.RecoveryRequired, result.RepairSetID)
		}
		return nil
	},
}

func init() {
	syncJournalAliasesPlanCmd.Flags().IntVar(&syncJournalAliasRepairPlanLimit, "limit", defaultCLISyncJournalAliasRepairLimit, "Maximum orphan aliases selected for this reviewed batch")
	syncJournalAliasesReconcileBatchCmd.Flags().IntVar(&syncJournalAliasRepairBatchLimit, "limit", defaultCLISyncJournalAliasRepairLimit, "Same selection limit used by 'sync journal aliases plan'")
	syncJournalAliasesReconcileBatchCmd.Flags().StringVar(&syncJournalAliasExpectRepairSetID, "expect-repair-set-id", "", "Required repair_set_id from 'sync journal aliases plan'")
	syncJournalAliasesCmd.AddCommand(syncJournalAliasesPlanCmd, syncJournalAliasesReconcileBatchCmd)
}
