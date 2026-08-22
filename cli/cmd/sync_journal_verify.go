package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/spf13/cobra"
)

const (
	syncJournalVerificationSchema       = "115driver.sync-journal-verification/v1"
	syncJournalDecisionAlreadyCompleted = "already-completed"
	syncJournalDecisionCompleted        = "completed"
	syncJournalDecisionRetry            = "retry"
	syncJournalDecisionError            = "error"
)

type syncJournalVerifyItem struct {
	Index        int    `json:"index"`
	RelativePath string `json:"relative_path"`
	Action       string `json:"action"`
	State        string `json:"state"`
	Phase        string `json:"phase"`
	Decision     string `json:"decision"`
	Error        string `json:"error,omitempty"`
}

type syncJournalVerification struct {
	Schema            string                  `json:"schema"`
	PlanID            string                  `json:"plan_id"`
	JournalVersion    int                     `json:"journal_version"`
	MigrationRequired bool                    `json:"migration_required"`
	JournalState      string                  `json:"journal_state"`
	Status            string                  `json:"status"`
	ResumeReady       bool                    `json:"resume_ready"`
	PreflightPassed   bool                    `json:"preflight_passed"`
	RecoveryRequired  bool                    `json:"recovery_required"`
	RecoveryClearable bool                    `json:"recovery_clearable,omitempty"`
	CompletedBefore   int                     `json:"completed_before"`
	CompletedDetected int                     `json:"completed_detected"`
	RetryFull         int                     `json:"retry_full"`
	WinnerOnly        int                     `json:"winner_only"`
	Pending           int                     `json:"pending"`
	Errors            int                     `json:"errors"`
	PreflightError    string                  `json:"preflight_error,omitempty"`
	Items             []syncJournalVerifyItem `json:"items"`
	VerifiedAt        time.Time               `json:"verified_at"`
}

func verifySyncJournalResume(ctx context.Context, planClient syncPlanClient, journal syncExecutionJournal) syncJournalVerification {
	if ctx == nil {
		ctx = context.Background()
	}
	result := syncJournalVerification{
		Schema: syncJournalVerificationSchema,
		PlanID: journal.PlanID, JournalVersion: journal.Version, MigrationRequired: journal.Version < syncJournalVersion,
		JournalState: journal.State, Status: syncJournalEffectiveStatus(journal), RecoveryRequired: journal.State == "recovery-required",
		Items: make([]syncJournalVerifyItem, 0, len(journal.Items)), VerifiedAt: time.Now().UTC(),
	}
	candidate := cloneSyncExecutionJournal(journal)
	for index := range candidate.Items {
		stored := &candidate.Items[index]
		item := candidate.Plan.Items[index]
		entry := syncJournalVerifyItem{
			Index: index, RelativePath: item.RelativePath, Action: item.Action, State: stored.State, Phase: stored.Phase,
		}
		if stored.State == "succeeded" || stored.State == "skipped" {
			entry.Decision = syncJournalDecisionAlreadyCompleted
			result.CompletedBefore++
			result.Items = append(result.Items, entry)
			continue
		}
		if err := ctx.Err(); err != nil {
			entry.Decision = syncJournalDecisionError
			entry.Error = err.Error()
			result.Errors++
			result.Items = append(result.Items, entry)
			continue
		}
		if syncPlanItemDestructive(item) {
			post, decision, err := reconcileSyncJournalDestructiveItem(ctx, planClient, candidate.Plan, item)
			if err != nil {
				entry.Decision = syncJournalDecisionError
				entry.Error = err.Error()
				result.Errors++
				result.Items = append(result.Items, entry)
				continue
			}
			entry.Decision = decision
			if decision == syncJournalDestructiveAmbiguous {
				entry.Error = "target no longer matches the planned loser or a verifiable winner"
				result.Errors++
				result.RecoveryRequired = true
			} else if err := syncjournalpkg.ApplyDestructiveDecision(&candidate, index, syncjournalpkg.DestructiveDecision(decision), post, time.Now().UTC()); err != nil {
				entry.Decision = syncJournalDecisionError
				entry.Error = err.Error()
				result.Errors++
			} else {
				switch decision {
				case syncJournalDestructiveCompleted:
					result.CompletedDetected++
				case syncJournalDestructiveRetryFull:
					result.RetryFull++
				case syncJournalDestructiveWinnerOnly:
					result.WinnerOnly++
				default:
					entry.Decision = syncJournalDecisionError
					entry.Error = fmt.Sprintf("invalid reconciliation decision %q", decision)
					result.Errors++
				}
			}
			result.Items = append(result.Items, entry)
			continue
		}

		post, completed, err := syncJournalPendingPostcondition(ctx, planClient, item)
		if err != nil {
			entry.Decision = syncJournalDecisionError
			entry.Error = err.Error()
			result.Errors++
			result.Items = append(result.Items, entry)
			continue
		}
		if completed {
			entry.Decision = syncJournalDecisionCompleted
			stored.State = "succeeded"
			stored.Phase = syncjournalpkg.PhaseDone
			stored.Post = post
			stored.LastError = ""
			result.CompletedDetected++
			result.Items = append(result.Items, entry)
			continue
		}
		if stored.Phase == syncjournalpkg.PhaseMutationDone {
			entry.Decision = syncJournalDecisionError
			entry.Error = "completed mutation postcondition is not yet verifiable"
			result.Errors++
			result.Items = append(result.Items, entry)
			continue
		}
		entry.Decision = syncJournalDecisionRetry
		stored.State = "pending"
		if stored.Phase != syncjournalpkg.PhaseMutationStarted {
			stored.Phase = syncjournalpkg.PhasePending
		}
		stored.Post = nil
		stored.LastError = ""
		result.Pending++
		result.Items = append(result.Items, entry)
	}

	if result.Errors == 0 {
		candidate.State = "active"
		candidate.LastError = ""
		candidate.CompletedAt = nil
		if err := preflightSyncJournalResume(ctx, planClient, candidate); err != nil {
			result.PreflightError = err.Error()
		} else {
			result.PreflightPassed = true
			if result.RecoveryRequired {
				result.RecoveryClearable = true
			} else {
				result.ResumeReady = true
			}
		}
	}
	return result
}

func printSyncJournalVerification(result syncJournalVerification) {
	if jsonOutput {
		return
	}
	fmt.Printf("Sync journal verify: %s\n", result.PlanID)
	fmt.Printf("Schema: v%d; migration-required=%t\n", result.JournalVersion, result.MigrationRequired)
	fmt.Printf("State: %s; status=%s; resume-ready=%t; recovery-clearable=%t; preflight=%t\n", result.JournalState, result.Status, result.ResumeReady, result.RecoveryClearable, result.PreflightPassed)
	fmt.Printf("Decisions: completed-before=%d detected=%d retry=%d retry-full=%d winner-only=%d errors=%d\n",
		result.CompletedBefore, result.CompletedDetected, result.Pending, result.RetryFull, result.WinnerOnly, result.Errors)
	for _, item := range result.Items {
		extra := ""
		if item.Error != "" {
			extra = " [" + item.Error + "]"
		}
		fmt.Printf("%-13s %-15s %s%s\n", item.Decision, item.Action, item.RelativePath, extra)
	}
	if result.PreflightError != "" {
		fmt.Printf("Preflight: %s\n", result.PreflightError)
	}
}

var syncJournalVerifyCmd = &cobra.Command{
	Use:   "verify <plan_id>",
	Short: "Read-only verification of whether a sync journal can be safely resumed",
	Long:  "Read-only verification of current local/remote state without mutating the journal or either tree. Interrupted destructive actions are classified from evidence as completed, retry-full, winner-only, or ambiguous; pending non-destructive actions are checked for already-completed postconditions. If every action is classifiable, the same mixed whole-tree resume preflight is run. The command exits non-zero unless the journal is safe to resume.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if err := validateSyncJournalAccountBinding(store); err != nil {
			return &exitError{code: output.ExitAuth, msg: err.Error()}
		}
		journal, err := store.Inspect(args[0])
		if err != nil {
			return syncJournalExitError(err)
		}
		result := verifySyncJournalResume(cmd.Context(), client, journal)
		if !result.ResumeReady {
			message := "sync journal is not currently safe to resume"
			if result.RecoveryClearable {
				message += "; reviewed evidence is consistent, run 'sync journal recover " + journal.PlanID[:12] + "' to clear the recovery latch"
			} else if result.RecoveryRequired {
				message += "; destructive state still requires manual recovery review"
			} else if strings.TrimSpace(result.PreflightError) != "" {
				message += ": " + result.PreflightError
			}
			if !jsonOutput {
				printSyncJournalVerification(result)
			}
			return &exitError{code: output.ExitError, msg: message, data: result}
		}
		printer.PrintSuccess(result)
		printSyncJournalVerification(result)
		return nil
	},
}
