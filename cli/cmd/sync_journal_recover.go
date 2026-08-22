package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

const syncJournalRecoveryResultSchema = "115driver.sync-journal-recovery/v1"

type syncJournalRecoveryResult struct {
	Schema         string                  `json:"schema"`
	PlanID         string                  `json:"plan_id"`
	Version        int                     `json:"version"`
	PreviousState  string                  `json:"previous_state"`
	PreviousStatus string                  `json:"previous_status"`
	State          string                  `json:"state"`
	Status         string                  `json:"status"`
	Verification   syncJournalVerification `json:"verification"`
}

func newSyncJournalRecoveryResult(before, after syncExecutionJournal, verification syncJournalVerification) syncJournalRecoveryResult {
	return syncJournalRecoveryResult{
		Schema: syncJournalRecoveryResultSchema,
		PlanID: after.PlanID, Version: after.Version, PreviousState: before.State, PreviousStatus: before.Status,
		State: after.State, Status: after.Status, Verification: verification,
	}
}

func openSyncRecoveryJournal(store syncJournalStore, prefix string) (*syncJournalHandle, error) {
	preview, err := store.Inspect(prefix)
	if err != nil {
		return nil, syncJournalExitError(err)
	}
	if preview.State != syncJournalStatusRecoveryRequired {
		return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("sync journal %s is %s, not recovery-required", preview.PlanID, preview.State)}
	}
	handle, err := store.Open(prefix)
	if err != nil {
		return nil, syncJournalExitError(err)
	}
	if current := handle.snapshot(); current.State != syncJournalStatusRecoveryRequired {
		_ = handle.Close()
		return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("sync journal %s changed to %s before recovery lock was acquired", current.PlanID, current.State)}
	}
	return handle, nil
}

var syncJournalRecoverCmd = &cobra.Command{
	Use:   "recover <plan_id>",
	Short: "Clear a reviewed recovery latch when current evidence is safe",
	Long:  "Re-evaluate a recovery-required sync journal against the current local and remote trees. This command never executes sync data actions; it only updates the journal. The recovery latch is cleared only when destructive outcomes can be classified and the full mixed resume preflight passes. If evidence remains ambiguous or the tree drifted, the journal stays recovery-required.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if err := validateSyncJournalAccountBinding(store); err != nil {
			return &exitError{code: output.ExitAuth, msg: err.Error()}
		}
		handle, err := openSyncRecoveryJournal(store, args[0])
		if err != nil {
			return err
		}
		defer handle.Close()
		before := handle.snapshot()
		verification := verifySyncJournalResume(cmd.Context(), client, before)
		if !verification.RecoveryClearable {
			message := "sync journal recovery latch cannot be cleared because current evidence is not fully safe"
			if verification.PreflightError != "" {
				message += ": " + verification.PreflightError
			}
			return &exitError{code: output.ExitError, msg: message, data: verification}
		}
		if err := handle.reconcileForResumeAfterReview(cmd.Context(), client); err != nil {
			return &exitError{code: syncJournalExitCode(err), msg: fmt.Sprintf("recover sync journal: %v", err), data: verification}
		}
		after := handle.snapshot()
		result := newSyncJournalRecoveryResult(before, after, verification)
		printer.PrintSuccess(result)
		if !jsonOutput {
			fmt.Printf("Sync journal recovery latch cleared: %s (%s -> %s)\n", after.PlanID, before.State, after.State)
			fmt.Printf("No data actions were executed; resume with: 115driver sync --resume %s %s %s\n", after.PlanID[:12], after.Plan.LocalRoot, after.Plan.RemoteRoot)
		}
		return nil
	},
}
