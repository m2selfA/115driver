package cmd

import (
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

var (
	syncDryRun           bool
	syncCheck            bool
	syncAllowDestructive bool
	syncContinueOnError  bool
	syncDelete           bool
	syncExpectPlan       string
	syncResume           string
	syncNoJournal        bool
	syncMaxDeleteRoots   int
	syncMaxDeleteItems   int
	syncMaxDeleteBytes   string
	syncMaxErrors        int
	syncJobs             = 1
	syncDirection        = syncDirectionBoth
	syncConflictPolicy   = syncConflictError
)

func syncInputArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(2)(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, err := resolveSyncJobs(syncJobs); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateSyncFailurePolicy(syncContinueOnError, syncMaxErrors); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if _, err := normalizeSyncPlanID(syncExpectPlan); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	deleteBudget, err := resolveSyncDeleteBudgetWithItems(syncMaxDeleteRoots, syncMaxDeleteItems, syncMaxDeleteBytes)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateSyncResumeFlagContract(cmd, syncResume); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if strings.TrimSpace(syncResume) != "" {
		return nil
	}
	if err := validateSyncDeleteBudgetUsage(syncDelete, deleteBudget); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if _, err := resolveSyncPlanOptionsWithDelete(syncDirection, syncConflictPolicy, syncDelete); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return nil
}

var syncCmd = &cobra.Command{
	Use:     "sync <local_dir> <remote_dir>",
	Short:   "Synchronize local and remote directory trees from an explicit plan",
	Long:    "Compare an existing local directory with an existing remote directory and execute the resulting deterministic plan. --dry-run prints the same plan without changing data. --check is also read-only, prints the plan in text mode, and exits non-zero when the plan contains any transfer/replacement/mirror-delete action or unresolved conflict; a fully converged tree exits zero. Executions persist a sync journal by default under the managed transfer session directory. The journal stores the reviewed plan, per-action attempts/phases, and verified postconditions so a partial run can be continued with --resume <plan_id>. Resume uses the stored policy and performs a mixed whole-tree preflight: completed actions must still satisfy their recorded postconditions while pending actions must still satisfy the original plan preconditions. --no-journal explicitly opts out. Destructive phases are fail-closed: an interrupted replacement/delete whose mutation outcome cannot be proven is marked recovery-required and is never silently replayed; inspect it with 'sync journal inspect'. --direction controls which transfer directions may be planned. By default entries that exist only on the excluded side are preserved. --delete enables explicit one-way mirror deletion and therefore requires --direction upload or download: upload deletes remote-only entries, while download deletes local-only entries. Directory mirror deletion is collapsed to one destructive root action; descendants remain covered plan items for audit/preflight and are not deleted twice. --conflict error keeps divergent/type-mismatched entries unresolved; prefer-local and prefer-remote resolve them into explicit destructive replace-remote/replace-local actions when the selected direction permits that policy. Execution refuses unresolved conflicts and independently refuses every destructive replacement or mirror deletion unless --allow-destructive is set. Optional --max-delete-roots N, --max-delete-items N, and --max-delete-bytes SIZE add execution-only guardrails for mirror deletion; directory subtrees count as one collapsed delete root while every affected descendant file/directory counts toward the item budget and descendant file bytes count toward the byte budget. Every successful plan carries a deterministic plan_id derived from roots, policy, actions, object identities, sizes/checksums, and captured modification times. --expect-plan <plan_id> makes fresh execution or resume fail unless it matches the reviewed plan. Before transfer configuration is prepared or any write begins, execution performs a whole-tree read-only preflight; any stale-tree failure aborts with zero processed actions. Every action then repeats its stale-plan checks immediately before mutation. --jobs N runs up to N dependency-ready actions concurrently using a deterministic dependency DAG: directory creation, directory replacement, and destructive mirror roots are barriers for covered descendant work; destructive actions in the same parent directory are serialized; and, by default, a failed execution wave prevents later waves from starting. --continue-on-error changes only that failure policy: dependency-blocked descendants are marked blocked while independent DAG branches continue; the command still returns failure with a partial summary. --max-errors N optionally stops that mode from launching another wave after N or more failures have accumulated (an already-running wave is always allowed to finish). Concurrent file transfers share the configured workers-per-interface budget instead of multiplying it by N. Destructive actions are not atomic: after revalidation a replacement removes the planned loser before creating/transferring the winner, while a mirror deletion moves the planned target to the remote recycle bin or removes the planned local target. Identical files are skipped only after exact SHA1 verification, so same-size files may be read completely from local disk while planning. No timestamp-based overwrite direction is guessed. File transfers reuse the existing transfer configuration and managed Session Store v2 resume behavior.",
	Example: "  115driver sync --dry-run ./photos /backup/photos\n  115driver sync --jobs 4 ./photos /backup/photos\n  115driver sync --resume <plan_id> --jobs 4 ./photos /backup/photos\n  115driver sync journal list\n  115driver sync journal inspect <plan_id>\n  115driver sync --direction upload --delete --dry-run ./photos /backup/photos\n  115driver sync --direction upload --delete --allow-destructive --expect-plan <plan_id> --jobs 4 ./photos /backup/photos\n  115driver sync --conflict prefer-local --allow-destructive --jobs 4 ./photos /backup/photos\n  115driver sync --dry-run --json ./photos /backup/photos",
	Args:    syncInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		jobs, err := resolveSyncJobs(syncJobs)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if err := validateSyncFailurePolicy(syncContinueOnError, syncMaxErrors); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		expectedPlanID, err := normalizeSyncPlanID(syncExpectPlan)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		deleteBudget, err := resolveSyncDeleteBudgetWithItems(syncMaxDeleteRoots, syncMaxDeleteItems, syncMaxDeleteBytes)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if err := validateSyncResumeFlagContract(cmd, syncResume); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if strings.TrimSpace(syncResume) != "" {
			store, err := resolveSyncJournalStore()
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error()}
			}
			if err := validateSyncJournalAccountBinding(store); err != nil {
				return &exitError{code: output.ExitAuth, msg: fmt.Sprintf("resume sync journal: %v", err)}
			}
			handle, err := openSyncResumeJournal(store, syncResume, args[0], args[1], expectedPlanID, deleteBudget)
			if err != nil {
				return err
			}
			return runSyncResumeExecution(cmd, handle, syncAllowDestructive, jobs, syncContinueOnError, syncMaxErrors)
		}
		if err := validateSyncDeleteBudgetUsage(syncDelete, deleteBudget); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		options, err := resolveSyncPlanOptionsWithDelete(syncDirection, syncConflictPolicy, syncDelete)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		plan, err := buildSyncPlanWithOptions(client, args[0], args[1], options)
		if err != nil {
			return err
		}
		if syncCheck {
			if err := validateSyncExpectedPlanID(plan, expectedPlanID); err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error(), data: plan}
			}
			if err := validateSyncCheck(plan); err != nil {
				if !jsonOutput {
					printSyncPlan(plan)
				}
				return &exitError{code: output.ExitError, msg: err.Error(), data: plan}
			}
			printer.PrintSuccess(plan)
			printSyncPlan(plan)
			return nil
		}
		if !syncDryRun {
			if err := validateSyncExpectedPlanID(plan, expectedPlanID); err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error(), data: plan}
			}
			if err := validateSyncDeleteBudget(plan, deleteBudget); err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error(), data: plan}
			}
		}
		if syncDryRun {
			printer.PrintSuccess(plan)
			printSyncPlan(plan)
			return nil
		}
		return runSyncExecutionWithManagedJournal(cmd, plan, syncAllowDestructive, jobs, syncContinueOnError, syncMaxErrors, !syncNoJournal)
	},
}

func init() {
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Compare both directory trees and print the sync plan without changing data")
	syncCmd.Flags().BoolVar(&syncCheck, "check", false, "Read-only drift check: exit non-zero when sync would change data or has unresolved conflicts")
	syncCmd.Flags().BoolVar(&syncAllowDestructive, "allow-destructive", false, "Allow explicit destructive replacement and mirror-delete plan actions during execution")
	syncCmd.Flags().BoolVar(&syncContinueOnError, "continue-on-error", false, "Continue independent DAG branches after failures while blocking dependent descendants")
	syncCmd.Flags().IntVar(&syncMaxErrors, "max-errors", 0, "With --continue-on-error, stop before the next wave after N failures (0 = unlimited)")
	syncCmd.Flags().BoolVar(&syncDelete, "delete", false, "Mirror one-way sync by deleting target-side-only entries (requires --direction upload or download and --allow-destructive for execution)")
	syncCmd.Flags().StringVar(&syncExpectPlan, "expect-plan", "", "Require execution to match this reviewed deterministic plan ID")
	syncCmd.Flags().StringVar(&syncResume, "resume", "", "Resume a persisted sync execution journal by plan ID or unique prefix")
	syncCmd.Flags().BoolVar(&syncNoJournal, "no-journal", false, "Disable persistent sync execution journaling for this fresh execution")
	syncCmd.Flags().IntVar(&syncMaxDeleteRoots, "max-delete-roots", 0, "Refuse execution when mirror deletion exceeds N collapsed roots (0 = unlimited)")
	syncCmd.Flags().IntVar(&syncMaxDeleteItems, "max-delete-items", 0, "Refuse execution when mirror deletion affects more than N files/directories including covered descendants (0 = unlimited)")
	syncCmd.Flags().StringVar(&syncMaxDeleteBytes, "max-delete-bytes", "", "Refuse execution when mirror deletion exceeds SIZE bytes across deleted file subtrees (empty = unlimited)")
	syncCmd.Flags().IntVar(&syncJobs, "jobs", 1, "Run up to N dependency-ready sync actions concurrently")
	syncCmd.Flags().StringVar(&syncDirection, "direction", syncDirectionBoth, "Plan transfer direction: both, upload, or download (excluded-side-only entries are preserved)")
	syncCmd.Flags().StringVar(&syncConflictPolicy, "conflict", syncConflictError, "Resolve divergent entries: error, prefer-local, or prefer-remote")
	rootCmd.AddCommand(syncCmd)
}
