package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/spf13/cobra"
)

const maxCLISyncJournalTrashScan = 4096

type syncJournalTrashEntry struct {
	PlanID               string    `json:"plan_id"`
	State                string    `json:"state"`
	Status               string    `json:"status"`
	TrashedAt            time.Time `json:"trashed_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	TrashAgeMillis       int64     `json:"trash_age_ms"`
	TrashRetentionMillis int64     `json:"trash_retention_ms"`
	PurgeEligibleAt      time.Time `json:"purge_eligible_at"`
	PurgeEligible        bool      `json:"purge_eligible"`
	TotalItems           int       `json:"total_items"`
	Runs                 int       `json:"runs"`
	ResumeRuns           int       `json:"resume_runs"`
	RecoveryRequired     bool      `json:"recovery_required"`
	ReconcileRequired    bool      `json:"reconcile_required"`
}

func (store syncJournalStore) sharedCurrentStore() syncjournalpkg.Store {
	return syncjournalpkg.Store{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		AutoGC: store.AutoGC, GCInterval: store.GCInterval,
		Retention: store.Retention, TrashRetention: store.TrashRetention,
	}
}

func syncJournalTrashEntryFromRecord(record syncjournalpkg.TrashedCurrentRecord, trashRetention time.Duration, now time.Time) syncJournalTrashEntry {
	journal := record.Journal
	window := syncjournalpkg.BuildTrashRetentionWindow(record.TrashedAt, now, trashRetention)
	return syncJournalTrashEntry{
		PlanID: journal.PlanID, State: journal.State, Status: journal.Status,
		TrashedAt: record.TrashedAt, UpdatedAt: journal.UpdatedAt,
		TrashAgeMillis: window.Age.Milliseconds(), TrashRetentionMillis: window.Retention.Milliseconds(),
		PurgeEligibleAt: window.EligibleAt, PurgeEligible: window.Eligible,
		TotalItems: len(journal.Items), Runs: journal.RunStats.Runs, ResumeRuns: journal.RunStats.ResumeRuns,
		RecoveryRequired:  syncjournalpkg.RecoveryRequired(journal),
		ReconcileRequired: syncjournalpkg.DestructiveReconciliationRequired(journal) || syncjournalpkg.PostconditionVerificationRequired(journal),
	}
}

func resolveSyncJournalTrashRecord(store syncjournalpkg.Store, prefix string) (syncjournalpkg.TrashedCurrentRecord, error) {
	prefix, err := normalizeSyncJournalPrefix(prefix)
	if err != nil {
		return syncjournalpkg.TrashedCurrentRecord{}, err
	}
	scan, err := store.ScanTrashedCurrent(maxCLISyncJournalTrashScan)
	if err != nil {
		return syncjournalpkg.TrashedCurrentRecord{}, err
	}
	matches := make([]syncjournalpkg.TrashedCurrentRecord, 0, 1)
	for _, record := range scan.Records {
		if strings.HasPrefix(record.Journal.PlanID, prefix) {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return syncjournalpkg.TrashedCurrentRecord{}, errSyncJournalNotFound
	}
	if len(matches) > 1 {
		return syncjournalpkg.TrashedCurrentRecord{}, fmt.Errorf("trashed sync journal ID prefix %q is ambiguous", prefix)
	}
	return matches[0], nil
}

var syncJournalTrashCmd = &cobra.Command{
	Use:   "trash",
	Short: "Inspect and restore soft-deleted sync journals",
	Args:  cobra.NoArgs,
	Long:  "Inspect and restore current-v2 sync journals moved into the shared Session Store trash. Listing and restore are local control-plane operations and never execute sync data actions. Restore is serialized with sync-journal GC and bulk migration, requires a unique journal ID prefix, and fails closed if the canonical current journal has reappeared.",
}

var syncJournalTrashListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recoverable sync journals in Session Store trash",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		scan, err := store.sharedCurrentStore().ScanTrashedCurrent(maxCLISyncJournalTrashScan)
		if err != nil {
			return syncJournalExitError(err)
		}
		entries := make([]syncJournalTrashEntry, 0, len(scan.Records))
		now := time.Now().UTC()
		for _, record := range scan.Records {
			entries = append(entries, syncJournalTrashEntryFromRecord(record, store.TrashRetention, now))
		}
		printer.PrintSuccess(entries)
		if !jsonOutput {
			for _, entry := range entries {
				flags := ""
				if entry.RecoveryRequired {
					flags += " recovery-required"
				}
				if entry.ReconcileRequired {
					flags += " reconcile-required"
				}
				fmt.Printf("%s  %-18s items=%d trashed=%s purge-eligible=%s eligible=%t%s\n", entry.PlanID[:12], entry.Status, entry.TotalItems, entry.TrashedAt.UTC().Format(time.RFC3339), entry.PurgeEligibleAt.UTC().Format(time.RFC3339), entry.PurgeEligible, flags)
			}
			if len(entries) == 0 {
				fmt.Println("No recoverable sync journals in session trash.")
			}
			if scan.MigrationRequired > 0 || scan.Invalid > 0 {
				fmt.Printf("Skipped trash entries: migration-required=%d invalid=%d\n", scan.MigrationRequired, scan.Invalid)
			}
		}
		return nil
	},
}

var syncJournalTrashRestoreCmd = &cobra.Command{
	Use:   "restore <plan_id>",
	Short: "Restore one soft-deleted sync journal to current-v2 storage",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		shared := store.sharedCurrentStore()
		guard, err := shared.AcquireCleanupGuard()
		if err != nil {
			return syncJournalExitError(err)
		}
		defer guard.Close()
		record, err := resolveSyncJournalTrashRecord(shared, args[0])
		if err != nil {
			return syncJournalExitError(err)
		}
		journal, err := shared.RestoreTrashedCurrent(
			guard, record.TrashName, record.Journal.PlanID,
			record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs,
		)
		if err != nil {
			return syncJournalExitError(err)
		}
		entry := syncJournalTrashEntryFromRecord(syncjournalpkg.TrashedCurrentRecord{Journal: journal, TrashedAt: record.TrashedAt}, store.TrashRetention, time.Now().UTC())
		printer.PrintSuccess(entry)
		if !jsonOutput {
			fmt.Printf("Restored sync journal %s (%s).\n", journal.PlanID, journal.Status)
		}
		return nil
	},
}

func init() {
	syncJournalTrashCmd.AddCommand(syncJournalTrashListCmd, syncJournalTrashRestoreCmd)
	syncJournalCmd.AddCommand(syncJournalTrashCmd)
}
