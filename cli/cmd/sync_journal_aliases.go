package cmd

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/spf13/cobra"
)

const maxCLISyncJournalAliasScan = 4096

var (
	errSyncJournalAliasNotOrphan     = errors.New("sync journal review alias is not orphaned")
	errSyncJournalAliasRepairChanged = errors.New("sync journal review alias repair snapshot changed")
	syncJournalAliasExpectRepairID   string
)

type syncJournalAliasDiagnosisEntry struct {
	ReviewID string `json:"review_id"`
	Status   string `json:"status"`
	RepairID string `json:"repair_id,omitempty"`
	InUse    bool   `json:"in_use,omitempty"`
	Error    string `json:"error,omitempty"`
}

type syncJournalAliasDiagnosisReport struct {
	Scanned          int                              `json:"scanned"`
	Live             int                              `json:"live"`
	Orphan           int                              `json:"orphan"`
	SoftDeleted      int                              `json:"soft_deleted"`
	IdentityMismatch int                              `json:"identity_mismatch"`
	Invalid          int                              `json:"invalid"`
	Issues           int                              `json:"issues"`
	Entries          []syncJournalAliasDiagnosisEntry `json:"entries"`
}

type syncJournalAliasReconcileResult struct {
	ReviewID string `json:"review_id"`
	RepairID string `json:"repair_id"`
	Repaired bool   `json:"repaired"`
	Status   string `json:"status"`
}

func diagnoseCLISyncJournalAliases(store syncjournalpkg.Store) (syncJournalAliasDiagnosisReport, error) {
	scan, err := store.DiagnoseReviewAliasesProfile(maxCLISyncJournalAliasScan, maxCLISyncJournalAliasScan, nil)
	if err != nil {
		return syncJournalAliasDiagnosisReport{}, err
	}
	report := syncJournalAliasDiagnosisReport{
		Scanned: scan.Scanned, Live: scan.Live, Orphan: scan.Orphan, SoftDeleted: scan.SoftDeleted,
		IdentityMismatch: scan.IdentityMismatch, Invalid: scan.Invalid, Issues: scan.Issues,
		Entries: make([]syncJournalAliasDiagnosisEntry, 0, len(scan.Entries)),
	}
	for _, diagnosis := range scan.Entries {
		entry := syncJournalAliasDiagnosisEntry{
			ReviewID: diagnosis.Alias.ReviewID, Status: string(diagnosis.Status), InUse: diagnosis.InUse,
		}
		if diagnosis.Err != nil {
			entry.Error = diagnosis.Err.Error()
		}
		if diagnosis.Status == syncjournalpkg.ReviewAliasDiagnosisOrphan {
			entry.RepairID, err = syncjournalpkg.ReviewAliasRepairID(diagnosis.Alias)
			if err != nil {
				return syncJournalAliasDiagnosisReport{}, err
			}
		}
		report.Entries = append(report.Entries, entry)
	}
	return report, nil
}

func sameCLISyncJournalAliasRepairToken(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func reconcileCLISyncJournalAlias(store syncjournalpkg.Store, reviewID, expectRepairID string) (syncJournalAliasReconcileResult, error) {
	reviewID, err := syncjournalpkg.NormalizeReviewID(reviewID)
	if err != nil {
		return syncJournalAliasReconcileResult{}, err
	}
	expectRepairID, err = syncjournalpkg.NormalizeReviewID(expectRepairID)
	if err != nil {
		return syncJournalAliasReconcileResult{}, fmt.Errorf("invalid repair ID: %w", err)
	}
	scan, err := store.DiagnoseReviewAliasesProfile(maxCLISyncJournalAliasScan, maxCLISyncJournalAliasScan, nil)
	if err != nil {
		return syncJournalAliasReconcileResult{}, err
	}
	var found *syncjournalpkg.ReviewAliasDiagnosis
	for index := range scan.Entries {
		if scan.Entries[index].Alias.ReviewID == reviewID {
			found = &scan.Entries[index]
			break
		}
	}
	if found == nil {
		return syncJournalAliasReconcileResult{}, errSyncJournalNotFound
	}
	switch found.Status {
	case syncjournalpkg.ReviewAliasDiagnosisOrphan:
		// Continue through token and exact locked snapshot checks.
	case syncjournalpkg.ReviewAliasDiagnosisSoftDeleted:
		return syncJournalAliasReconcileResult{}, fmt.Errorf("%w: restore the soft-deleted journal instead of removing its alias", syncjournalpkg.ErrReviewAliasTrashed)
	case syncjournalpkg.ReviewAliasDiagnosisLive:
		return syncJournalAliasReconcileResult{}, fmt.Errorf("%w: reviewed alias still points to a live journal", errSyncJournalAliasNotOrphan)
	case syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch:
		return syncJournalAliasReconcileResult{}, fmt.Errorf("%w: reviewed alias target identity does not match", syncjournalpkg.ErrReviewAliasConflict)
	default:
		if found.Err != nil {
			return syncJournalAliasReconcileResult{}, found.Err
		}
		return syncJournalAliasReconcileResult{}, fmt.Errorf("%w: reviewed alias target cannot be proven safe", errSyncJournalInvalidSchema)
	}
	freshRepairID, err := syncjournalpkg.ReviewAliasRepairID(found.Alias)
	if err != nil {
		return syncJournalAliasReconcileResult{}, err
	}
	if !sameCLISyncJournalAliasRepairToken(freshRepairID, expectRepairID) {
		return syncJournalAliasReconcileResult{}, errSyncJournalAliasRepairChanged
	}

	repairStore := store
	repairStore.AccountID = found.Alias.AccountID
	removed, err := repairStore.RemoveOrphanReviewAliasExact(found.Alias)
	if err != nil {
		return syncJournalAliasReconcileResult{}, err
	}
	if !removed {
		return syncJournalAliasReconcileResult{}, errSyncJournalAliasRepairChanged
	}
	return syncJournalAliasReconcileResult{ReviewID: reviewID, RepairID: expectRepairID, Repaired: true, Status: "removed"}, nil
}

var syncJournalAliasesCmd = &cobra.Command{
	Use:   "aliases",
	Short: "Diagnose and reconcile private reviewed-plan aliases",
	Args:  cobra.NoArgs,
	Long:  "Diagnose and reconcile private reviewed-plan aliases without touching either sync tree. Diagnosis is read-only and issues a content-addressed repair token only for a proven orphan. Reconcile requires that exact token, re-diagnoses the alias, and removes it only after locked proof that neither current nor Session Store trash still contains the mapped journal.",
}

var syncJournalAliasesDiagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Read-only reviewed-plan alias lifecycle diagnosis",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		report, err := diagnoseCLISyncJournalAliases(store.sharedCurrentStore())
		if err != nil {
			return syncJournalExitError(err)
		}
		printer.PrintSuccess(report)
		if !jsonOutput {
			fmt.Printf("Sync journal aliases: scanned=%d live=%d orphan=%d soft-deleted=%d invalid=%d issues=%d\n", report.Scanned, report.Live, report.Orphan, report.SoftDeleted, report.Invalid, report.Issues)
			for _, entry := range report.Entries {
				flags := ""
				if entry.InUse {
					flags += " in-use"
				}
				if entry.Error != "" {
					flags += " [" + entry.Error + "]"
				}
				fmt.Printf("%s  %-20s%s\n", entry.ReviewID, entry.Status, flags)
				if entry.RepairID != "" {
					fmt.Printf("  repair_id=%s\n", entry.RepairID)
				}
			}
		}
		return nil
	},
}

func syncJournalAliasesReconcileArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, err := syncjournalpkg.NormalizeReviewID(args[0]); err != nil {
		return &exitError{code: output.ExitArgs, msg: "invalid review_id: " + err.Error()}
	}
	if strings.TrimSpace(syncJournalAliasExpectRepairID) == "" {
		return &exitError{code: output.ExitArgs, msg: "--expect-repair-id is required"}
	}
	if _, err := syncjournalpkg.NormalizeReviewID(syncJournalAliasExpectRepairID); err != nil {
		return &exitError{code: output.ExitArgs, msg: "invalid --expect-repair-id: " + err.Error()}
	}
	return nil
}

var syncJournalAliasesReconcileCmd = &cobra.Command{
	Use:   "reconcile <review_id>",
	Short: "Remove one exactly reviewed orphan alias",
	Args:  syncJournalAliasesReconcileArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		result, err := reconcileCLISyncJournalAlias(store.sharedCurrentStore(), args[0], syncJournalAliasExpectRepairID)
		if err != nil {
			return syncJournalExitErrorData(err, result)
		}
		printer.PrintSuccess(result)
		if !jsonOutput {
			fmt.Printf("Removed orphan sync journal alias %s using reviewed repair token %s.\n", result.ReviewID, result.RepairID)
		}
		return nil
	},
}

func init() {
	syncJournalAliasesReconcileCmd.Flags().StringVar(&syncJournalAliasExpectRepairID, "expect-repair-id", "", "Required repair_id from 'sync journal aliases diagnose'")
	syncJournalAliasesCmd.AddCommand(syncJournalAliasesDiagnoseCmd, syncJournalAliasesReconcileCmd)
	syncJournalCmd.AddCommand(syncJournalAliasesCmd)
}
