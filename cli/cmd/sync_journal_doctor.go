package cmd

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/spf13/cobra"
)

const (
	syncJournalDoctorHealthy            = "healthy"
	syncJournalDoctorMigrationRequired  = "migration-required"
	syncJournalDoctorInvalidSchema      = "invalid-schema"
	syncJournalDoctorNewerVersion       = "newer-version"
	syncJournalDoctorUnsupportedVersion = "unsupported-version"
	syncJournalDoctorBindingMismatch    = "binding-mismatch"
	syncJournalDoctorIOError            = "io-error"
	syncJournalDoctorInvalidPath        = "invalid-path"
	syncJournalBackupNotRequired        = "not-required"
	syncJournalBackupOK                 = "ok"
	syncJournalBackupMissing            = "missing"
	syncJournalBackupInvalid            = "invalid"
)

const (
	syncJournalDoctorAliasLive          = "live"
	syncJournalDoctorAliasOrphan        = "orphan"
	syncJournalDoctorAliasSoftDeleted   = "soft-deleted-shadow"
	syncJournalDoctorAliasInvalidTarget = "invalid-target"
	maxSyncJournalDoctorAliasScan       = 4096
)

type syncJournalDoctorEntry struct {
	PlanID                   string `json:"plan_id,omitempty"`
	JournalPath              string `json:"journal_path"`
	Version                  int    `json:"version,omitempty"`
	Schema                   string `json:"schema,omitempty"`
	State                    string `json:"state,omitempty"`
	Status                   string `json:"status,omitempty"`
	Health                   string `json:"health"`
	MigrationRequired        bool   `json:"migration_required,omitempty"`
	InUse                    bool   `json:"in_use,omitempty"`
	MigrationBackupStatus    string `json:"migration_backup_status,omitempty"`
	MigrationBackupsRequired int    `json:"migration_backups_required,omitempty"`
	MigrationBackupsMissing  int    `json:"migration_backups_missing,omitempty"`
	MigrationBackupsInvalid  int    `json:"migration_backups_invalid,omitempty"`
	MigrationBackupError     string `json:"migration_backup_error,omitempty"`
	Error                    string `json:"error,omitempty"`
	SuggestedAction          string `json:"suggested_action,omitempty"`
}

type syncJournalDoctorReviewAliasEntry struct {
	ReviewID        string `json:"review_id"`
	PlanID          string `json:"plan_id"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	SuggestedAction string `json:"suggested_action,omitempty"`
}

type syncJournalDoctorReport struct {
	Schema                    string                               `json:"schema"`
	CurrentVersion            int                                  `json:"current_version"`
	MinimumReadable           int                                  `json:"minimum_readable_version"`
	Total                     int                                  `json:"total"`
	Healthy                   int                                  `json:"healthy"`
	MigrationRequired         int                                  `json:"migration_required"`
	Issues                    int                                  `json:"issues"`
	Warnings                  int                                  `json:"warnings"`
	MigrationBackupsMissing   int                                  `json:"migration_backups_missing"`
	MigrationBackupsInvalid   int                                  `json:"migration_backups_invalid"`
	InUse                     int                                  `json:"in_use"`
	ReviewAliasesTotal        int                                  `json:"review_aliases_total"`
	ReviewAliasesLive         int                                  `json:"review_aliases_live"`
	ReviewAliasesOrphan       int                                  `json:"review_aliases_orphan"`
	ReviewAliasesSoftDeleted  int                                  `json:"review_aliases_soft_deleted"`
	ReviewAliasScanError      string                               `json:"review_alias_scan_error,omitempty"`
	ReviewAliases             []syncJournalDoctorReviewAliasEntry  `json:"review_aliases,omitempty"`
	AllCurrentAndValid        bool                                 `json:"all_current_and_valid"`
	InterruptedMigrationBatch bool                                 `json:"interrupted_migration_batch,omitempty"`
	MigrationBatch            *syncJournalMigrationBatchDiagnostic `json:"migration_batch,omitempty"`
	Entries                   []syncJournalDoctorEntry             `json:"entries"`
}

func classifySyncJournalDoctorError(err error) (health, action string) {
	switch {
	case errors.Is(err, errSyncJournalNewerVersion):
		return syncJournalDoctorNewerVersion, "upgrade 115driver before opening this journal"
	case errors.Is(err, errSyncJournalUnsupportedVersion):
		return syncJournalDoctorUnsupportedVersion, "use a 115driver version that supports this journal schema"
	case errors.Is(err, errSyncJournalInvalidSchema):
		return syncJournalDoctorInvalidSchema, "inspect or restore the journal from a known-good copy; do not resume it"
	default:
		return syncJournalDoctorInvalidSchema, "inspect or restore the journal from a known-good copy; do not resume it"
	}
}

func validSyncJournalStoragePlanID(planID string) bool {
	if len(planID) != 64 {
		return false
	}
	_, err := hex.DecodeString(planID)
	return err == nil
}

func sameSyncJournalPath(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr != nil || bErr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}

func diagnoseSyncJournalMigrationBackups(location syncJournalLocation, journal syncExecutionJournal) (status string, required, missing, invalid int, message string) {
	messages := make([]string, 0)
	for _, record := range journal.Migrations {
		if !record.BackupRequired {
			continue
		}
		required++
		_, err := readSyncJournalMigrationBackup(location, record)
		if err == nil {
			continue
		}
		if os.IsNotExist(err) {
			missing++
			messages = append(messages, fmt.Sprintf("v%d -> v%d backup is missing", record.FromVersion, record.ToVersion))
			continue
		}
		invalid++
		messages = append(messages, fmt.Sprintf("v%d -> v%d backup: %v", record.FromVersion, record.ToVersion, err))
	}
	switch {
	case required == 0:
		status = syncJournalBackupNotRequired
	case invalid > 0:
		status = syncJournalBackupInvalid
	case missing > 0:
		status = syncJournalBackupMissing
	default:
		status = syncJournalBackupOK
	}
	return status, required, missing, invalid, strings.Join(messages, "; ")
}

func (store syncJournalStore) diagnoseReviewAliases(report *syncJournalDoctorReport) {
	if report == nil {
		return
	}
	shared := store.sharedCurrentStore()
	scan, err := shared.DiagnoseReviewAliasesProfile(maxSyncJournalDoctorAliasScan, maxSyncJournalDoctorAliasScan, nil)
	if err != nil {
		report.ReviewAliasScanError = err.Error()
		report.Issues++
		report.AllCurrentAndValid = false
		return
	}
	report.ReviewAliasesTotal = scan.Scanned
	report.ReviewAliasesLive = scan.Live
	report.ReviewAliasesOrphan = scan.Orphan
	report.ReviewAliasesSoftDeleted = scan.SoftDeleted
	report.Issues += scan.Issues
	if scan.Issues > 0 {
		report.AllCurrentAndValid = false
	}
	report.ReviewAliases = make([]syncJournalDoctorReviewAliasEntry, 0, len(scan.Entries))
	for _, diagnosis := range scan.Entries {
		alias := diagnosis.Alias
		entry := syncJournalDoctorReviewAliasEntry{ReviewID: alias.ReviewID, PlanID: alias.PlanID, Status: string(diagnosis.Status)}
		if diagnosis.Err != nil {
			entry.Error = diagnosis.Err.Error()
		}
		switch diagnosis.Status {
		case syncjournalpkg.ReviewAliasDiagnosisOrphan:
			entry.SuggestedAction = "review and remove the orphan alias before reusing this reviewed plan ID"
		case syncjournalpkg.ReviewAliasDiagnosisSoftDeleted:
			entry.SuggestedAction = "restore the soft-deleted journal or complete manual alias repair before reusing this reviewed plan"
		case syncjournalpkg.ReviewAliasDiagnosisInvalidTarget, syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch:
			entry.SuggestedAction = "inspect the target journal and alias binding before any resume or repair"
		}
		report.ReviewAliases = append(report.ReviewAliases, entry)
	}
	sort.Slice(report.ReviewAliases, func(i, j int) bool {
		return report.ReviewAliases[i].ReviewID < report.ReviewAliases[j].ReviewID
	})
}

func (store syncJournalStore) Diagnose() (syncJournalDoctorReport, error) {
	report := syncJournalDoctorReport{
		Schema: syncJournalSchemaID, CurrentVersion: syncJournalVersion, MinimumReadable: syncJournalMinReadableVersion,
		AllCurrentAndValid: true, Entries: make([]syncJournalDoctorEntry, 0),
	}
	root, err := store.root()
	if err != nil {
		return report, err
	}
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "journal.json" {
			return nil
		}
		rel, relErr := filepath.Rel(root, current)
		if relErr != nil {
			rel = current
		}
		diagnostic := syncJournalDoctorEntry{JournalPath: filepath.ToSlash(rel)}
		planID := strings.ToLower(filepath.Base(filepath.Dir(current)))
		diagnostic.PlanID = planID
		report.Total++

		if !validSyncJournalStoragePlanID(planID) {
			diagnostic.Health = syncJournalDoctorInvalidPath
			diagnostic.Error = "journal is not stored under a 64-character hexadecimal plan ID directory"
			diagnostic.SuggestedAction = "move the journal aside for manual inspection; it cannot be addressed safely by plan ID"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		location, locErr := store.location(planID)
		if locErr != nil || !sameSyncJournalPath(current, location.JournalPath) {
			diagnostic.Health = syncJournalDoctorInvalidPath
			diagnostic.Error = "journal path does not match the canonical plan ID shard"
			diagnostic.SuggestedAction = "move the journal aside for manual inspection; do not resume it from this path"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		diagnostic.InUse, _ = transfer.SessionLockInUse(location.LockPath)
		if diagnostic.InUse {
			report.InUse++
		}

		data, readErr := os.ReadFile(current)
		if readErr != nil {
			diagnostic.Health = syncJournalDoctorIOError
			diagnostic.Error = readErr.Error()
			diagnostic.SuggestedAction = "check filesystem permissions and journal storage health"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		var header struct {
			Version int    `json:"version"`
			Schema  string `json:"schema"`
			State   string `json:"state"`
			Status  string `json:"status"`
		}
		if headerErr := json.Unmarshal(data, &header); headerErr != nil {
			diagnostic.Health = syncJournalDoctorInvalidSchema
			diagnostic.Error = "decode journal header: " + headerErr.Error()
			diagnostic.SuggestedAction = "restore the journal from a known-good copy; do not resume it"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		diagnostic.Version = header.Version
		diagnostic.Schema = header.Schema
		diagnostic.State = header.State
		diagnostic.Status = header.Status

		journal, decodeErr := decodeSyncJournalData(data)
		if decodeErr != nil {
			diagnostic.Health, diagnostic.SuggestedAction = classifySyncJournalDoctorError(decodeErr)
			diagnostic.Error = decodeErr.Error()
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		if journal.PlanID != planID {
			diagnostic.Health = syncJournalDoctorInvalidSchema
			diagnostic.Error = fmt.Sprintf("journal plan_id %q does not match storage path %q", journal.PlanID, planID)
			diagnostic.SuggestedAction = "restore the journal to its canonical plan ID path; do not resume it here"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		if bindingErr := store.validateJournalBinding(journal); bindingErr != nil {
			diagnostic.Health = syncJournalDoctorBindingMismatch
			diagnostic.Error = bindingErr.Error()
			diagnostic.SuggestedAction = "use the profile/account that created this journal"
			report.Issues++
			report.AllCurrentAndValid = false
			report.Entries = append(report.Entries, diagnostic)
			return nil
		}
		diagnostic.MigrationBackupStatus, diagnostic.MigrationBackupsRequired, diagnostic.MigrationBackupsMissing, diagnostic.MigrationBackupsInvalid, diagnostic.MigrationBackupError = diagnoseSyncJournalMigrationBackups(location, journal)
		report.MigrationBackupsMissing += diagnostic.MigrationBackupsMissing
		report.MigrationBackupsInvalid += diagnostic.MigrationBackupsInvalid
		report.Warnings += diagnostic.MigrationBackupsMissing + diagnostic.MigrationBackupsInvalid
		diagnostic.Status = syncJournalEffectiveStatus(journal)
		diagnostic.MigrationRequired = journal.Version < syncJournalVersion
		if diagnostic.MigrationRequired {
			diagnostic.Health = syncJournalDoctorMigrationRequired
			diagnostic.SuggestedAction = "run '115driver sync journal migrate " + journal.PlanID[:12] + "'"
			report.MigrationRequired++
			report.Issues++
			report.AllCurrentAndValid = false
		} else {
			diagnostic.Health = syncJournalDoctorHealthy
			report.Healthy++
		}
		report.Entries = append(report.Entries, diagnostic)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return report, err
	}
	store.diagnoseReviewAliases(&report)
	batch, batchErr := store.DiagnoseMigrationBatch()
	if batchErr != nil {
		return report, batchErr
	}
	if batch.Exists {
		report.MigrationBatch = &batch
		if batch.Interrupted {
			report.InterruptedMigrationBatch = true
			report.Issues++
			report.AllCurrentAndValid = false
		} else {
			report.Warnings++
		}
	}
	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].PlanID == report.Entries[j].PlanID {
			return report.Entries[i].JournalPath < report.Entries[j].JournalPath
		}
		return report.Entries[i].PlanID < report.Entries[j].PlanID
	})
	return report, nil
}

func printSyncJournalDoctorReport(report syncJournalDoctorReport) {
	if jsonOutput {
		return
	}
	fmt.Printf("Sync journal doctor: schema=%s current=v%d readable=v%d..v%d total=%d healthy=%d migration-required=%d issues=%d warnings=%d backup-missing=%d backup-invalid=%d in-use=%d aliases=%d live=%d orphan=%d soft-deleted=%d\n",
		report.Schema, report.CurrentVersion, report.MinimumReadable, report.CurrentVersion, report.Total, report.Healthy, report.MigrationRequired, report.Issues, report.Warnings, report.MigrationBackupsMissing, report.MigrationBackupsInvalid, report.InUse, report.ReviewAliasesTotal, report.ReviewAliasesLive, report.ReviewAliasesOrphan, report.ReviewAliasesSoftDeleted)
	if report.MigrationBatch != nil {
		batch := report.MigrationBatch
		fmt.Printf("Migration batch: id=%s state=%s in-use=%t interrupted=%t candidates=%d original=%d migrated=%d unknown=%d backup-issues=%d\n",
			batch.BatchID, batch.State, batch.InUse, batch.Interrupted, batch.Candidates, batch.Original, batch.Migrated, batch.Unknown, batch.BackupIssues)
		if batch.Interrupted {
			fmt.Println("  action: run '115driver sync journal migrate --recover-batch' before starting another bulk migration")
		}
	}
	if report.ReviewAliasScanError != "" {
		fmt.Printf("Review alias scan: ERROR [%s]\n", report.ReviewAliasScanError)
	}
	for _, alias := range report.ReviewAliases {
		reviewID := alias.ReviewID
		if len(reviewID) > 19 {
			reviewID = reviewID[:19]
		}
		planID := alias.PlanID
		if len(planID) > 12 {
			planID = planID[:12]
		}
		extra := ""
		if alias.Error != "" {
			extra = " [" + alias.Error + "]"
		}
		fmt.Printf("alias %-19s -> %-12s %-20s%s\n", reviewID, planID, alias.Status, extra)
		if alias.SuggestedAction != "" {
			fmt.Printf("  action: %s\n", alias.SuggestedAction)
		}
	}
	for _, entry := range report.Entries {
		id := entry.PlanID
		if len(id) > 12 {
			id = id[:12]
		}
		extra := ""
		if entry.InUse {
			extra += " in-use"
		}
		if entry.Error != "" {
			extra += " [" + entry.Error + "]"
		}
		if entry.MigrationBackupError != "" {
			extra += " [backup: " + entry.MigrationBackupError + "]"
		}
		fmt.Printf("%-12s %-20s v%-2d %-18s %s%s\n", id, entry.Health, entry.Version, entry.Status, entry.JournalPath, extra)
		if entry.SuggestedAction != "" {
			fmt.Printf("  action: %s\n", entry.SuggestedAction)
		}
	}
}

var syncJournalDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Read-only integrity and schema diagnosis for sync journals",
	Long:  "Read-only offline diagnosis of every sync journal and private reviewed-plan alias in the current profile store. It validates storage paths, schema/version envelopes, plan fingerprints, migration history, profile binding, journal state, required migration backup hashes, live/orphan/soft-deleted alias targets, and any bulk-migration batch crash marker without rewriting any journal or alias or touching either sync tree. Migration-required, invalid, orphan/soft-deleted alias, and interrupted bulk-migration states make the command exit non-zero. Missing or invalid historical migration backups are audit warnings for an otherwise valid journal; an in-use lock is reported but is not by itself corruption.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		report, err := store.Diagnose()
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}
		if report.Issues > 0 {
			if !jsonOutput {
				printSyncJournalDoctorReport(report)
			}
			return &exitError{code: output.ExitError, msg: fmt.Sprintf("sync journal doctor found %d issue(s)", report.Issues), data: report}
		}
		printer.PrintSuccess(report)
		printSyncJournalDoctorReport(report)
		return nil
	},
}
