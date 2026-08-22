package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func validateSyncJournalAccountBinding(store syncJournalStore) error {
	if store.AccountID <= 0 {
		return errors.New("sync execution journaling requires a known 115 account identity")
	}
	return nil
}

func runSyncExecutionWithManagedJournal(cmd *cobra.Command, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int, journalEnabled bool) error {
	if !journalEnabled || !syncPlanHasWrites(plan) {
		return runSyncExecutionWithFailurePolicy(cmd, plan, allowDestructive, requestedJobs, continueOnError, maxErrors)
	}
	jobs, err := resolveSyncJobs(requestedJobs)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, requestedJobs)}
	}
	if err := validateSyncFailurePolicy(continueOnError, maxErrors); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, jobs)}
	}
	if err := validateSyncExecutionSafety(plan, allowDestructive); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, jobs)}
	}
	store, err := resolveSyncJournalStore()
	if err != nil {
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("initialize sync journal store: %v", err)}
	}
	if err := validateSyncJournalAccountBinding(store); err != nil {
		return &exitError{code: output.ExitAuth, msg: fmt.Sprintf("initialize sync journal: %v; use --no-journal only if resumable sync execution is not required", err)}
	}
	handle, err := store.Create(plan)
	if err != nil {
		return syncJournalExitError(err)
	}
	defer handle.Close()
	if store.AutoGC {
		if maintenanceErr := runSyncJournalOpportunisticMaintenance(store); maintenanceErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: sync journal/session GC failed: %v\n", maintenanceErr)
		}
	}
	return runSyncExecutionWithJournalHandle(cmd, plan, allowDestructive, jobs, continueOnError, maxErrors, handle, false)
}

func runSyncResumeExecution(cmd *cobra.Command, handle *syncJournalHandle, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int) error {
	if handle == nil {
		return &exitError{code: output.ExitArgs, msg: "sync resume journal is nil"}
	}
	defer handle.Close()
	if err := handle.reconcileForResume(cmd.Context(), client); err != nil {
		return syncJournalExitError(err)
	}
	journal := handle.snapshot()
	residual := buildSyncJournalResidualPlan(journal)
	return runSyncExecutionWithJournalHandle(cmd, residual, allowDestructive, requestedJobs, continueOnError, maxErrors, handle, true)
}

func runSyncExecutionWithJournalHandle(cmd *cobra.Command, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int, handle *syncJournalHandle, resumed bool) error {
	jobs, err := resolveSyncJobs(requestedJobs)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, requestedJobs)}
	}
	if err := validateSyncFailurePolicy(continueOnError, maxErrors); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, jobs)}
	}
	if err := validateSyncExecutionSafety(plan, allowDestructive); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: newSyncExecutionSummaryWithJobs(plan, allowDestructive, jobs)}
	}
	if err := handle.beginRun(resumed); err != nil {
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("start sync journal run: %v", err)}
	}
	completedBefore := 0
	if resumed {
		for _, item := range handle.snapshot().Items {
			if item.State == "succeeded" {
				completedBefore++
			}
		}
	}
	deps, err := newSyncProductionExecutionDepsWithJobs(cmd, plan, jobs)
	if err != nil {
		initErr := fmt.Errorf("initialize sync execution: %w", err)
		summary := newSyncExecutionSummaryWithJobs(plan, allowDestructive, jobs)
		summary.JournalEnabled = true
		summary.JournalResumed = resumed
		summary.JournalCompletedBefore = completedBefore
		if journalErr := handle.finishRun(summary, initErr); journalErr != nil {
			initErr = errors.Join(initErr, fmt.Errorf("update sync execution journal: %w", journalErr))
		}
		journal := handle.snapshot()
		summary.JournalVersion = journal.Version
		summary.JournalState = journal.State
		summary.JournalStatus = journal.Status
		message := initErr.Error() + fmt.Sprintf("; resume journal %s with '115driver sync --resume %s <local_dir> <remote_dir>'", journal.PlanID, journal.PlanID)
		return &exitError{code: output.ExitError, msg: message, data: summary}
	}
	if resumed {
		deps.forcePreflight = true
		deps.preflight = func(ctx context.Context) error {
			return preflightSyncJournalResume(ctx, client, handle.snapshot())
		}
	}
	deps = attachSyncJournalExecutionDeps(handle, client, deps)
	summary, runErr := executeSyncPlanWithJobsFailurePolicy(cmd.Context(), plan, allowDestructive, jobs, continueOnError, maxErrors, deps)
	summary.JournalEnabled = true
	summary.JournalResumed = resumed
	summary.JournalCompletedBefore = completedBefore
	journalErr := handle.finishRun(summary, runErr)
	if journalErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("update sync execution journal: %w", journalErr))
	}
	journal := handle.snapshot()
	summary.JournalVersion = journal.Version
	summary.JournalState = journal.State
	summary.JournalStatus = journal.Status
	if runErr != nil {
		code := syncExecutionErrorCode(runErr)
		if errors.Is(runErr, errSyncJournalRecoveryRequired) {
			code = output.ExitArgs
		}
		message := runErr.Error()
		if journal.State == "recovery-required" {
			message += fmt.Sprintf("; sync journal %s requires manual review with '115driver sync journal inspect %s' before any retry", journal.PlanID, journal.PlanID)
		} else {
			message += fmt.Sprintf("; resume journal %s with '115driver sync --resume %s <local_dir> <remote_dir>'", journal.PlanID, journal.PlanID)
		}
		return &exitError{code: code, msg: message, data: summary}
	}
	printer.PrintSuccess(summary)
	printSyncExecutionSummary(summary)
	return nil
}

func validateSyncResumeJournalRequest(journal syncExecutionJournal, localRoot, remoteRoot, expectedPlanID string, deleteBudget syncDeleteBudget) error {
	if err := validateSyncResumeRoots(journal, localRoot, remoteRoot); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateSyncExpectedPlanID(journal.Plan, expectedPlanID); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: journal.Plan}
	}
	if err := validateSyncDeleteBudgetUsage(journal.Plan.DeleteExtraneous, deleteBudget); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateSyncDeleteBudget(journal.Plan, deleteBudget); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error(), data: journal.Plan}
	}
	return nil
}

func openSyncResumeJournal(store syncJournalStore, prefix, localRoot, remoteRoot, expectedPlanID string, deleteBudget syncDeleteBudget) (*syncJournalHandle, error) {
	preview, err := store.Inspect(prefix)
	if err != nil {
		return nil, syncJournalExitError(err)
	}
	if err := validateSyncResumeJournalRequest(preview, localRoot, remoteRoot, expectedPlanID, deleteBudget); err != nil {
		return nil, err
	}
	handle, err := store.Open(prefix)
	if err != nil {
		return nil, syncJournalExitError(err)
	}
	if err := validateSyncResumeJournalRequest(handle.snapshot(), localRoot, remoteRoot, expectedPlanID, deleteBudget); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return handle, nil
}

func validateSyncResumeRoots(journal syncExecutionJournal, localRoot, remoteRoot string) error {
	localAbs, err := filepath.Abs(localRoot)
	if err != nil {
		return fmt.Errorf("resolve sync resume local root: %w", err)
	}
	plannedLocal := filepath.Clean(journal.Plan.LocalRoot)
	requestedLocal := filepath.Clean(localAbs)
	if runtime.GOOS == "windows" {
		plannedLocal = strings.ToLower(plannedLocal)
		requestedLocal = strings.ToLower(requestedLocal)
	}
	if plannedLocal != requestedLocal {
		return fmt.Errorf("sync resume local root mismatch: journal=%q requested=%q", journal.Plan.LocalRoot, localAbs)
	}
	if canonicalSyncRemoteRoot(remoteRoot) != canonicalSyncRemoteRoot(journal.Plan.RemoteRoot) {
		return fmt.Errorf("sync resume remote root mismatch: journal=%q requested=%q", journal.Plan.RemoteRoot, remoteRoot)
	}
	return nil
}

func validateSyncResumeFlagContract(cmd *cobra.Command, resumeID string) error {
	if strings.TrimSpace(resumeID) == "" {
		return nil
	}
	if syncDryRun || syncCheck {
		return fmt.Errorf("--resume cannot be combined with --dry-run or --check; use sync journal inspect for read-only journal review")
	}
	if syncNoJournal {
		return fmt.Errorf("--resume cannot be combined with --no-journal")
	}
	for _, name := range []string{"direction", "conflict", "delete"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--resume cannot be combined with --%s; resume uses the stored plan policy", name)
		}
	}
	return nil
}
