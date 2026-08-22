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
	"sync"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/spf13/cobra"
)

const (
	syncJournalVersion                 = syncjournalpkg.Version
	syncJournalMinReadableVersion      = syncjournalpkg.MinReadableVersion
	syncJournalLayoutVersion           = syncjournalpkg.LayoutVersion
	syncJournalSchemaID                = syncjournalpkg.SchemaID
	syncJournalListEntrySchema         = syncjournalpkg.ListEntrySchema
	syncJournalStatusActive            = syncjournalpkg.StatusActive
	syncJournalStatusFailed            = syncjournalpkg.StatusFailed
	syncJournalStatusCompleted         = syncjournalpkg.StatusCompleted
	syncJournalStatusReconcileRequired = syncjournalpkg.StatusReconcileRequired
	syncJournalStatusRecoveryRequired  = syncjournalpkg.StatusRecoveryRequired
	syncJournalStatusUnknown           = syncjournalpkg.StatusUnknown
)

var (
	errSyncJournalExists             = errors.New("sync execution journal already exists")
	errSyncJournalNotFound           = errors.New("sync execution journal not found")
	errSyncJournalCompleted          = errors.New("sync execution journal is already completed")
	errSyncJournalRecoveryRequired   = errors.New("sync execution journal requires manual destructive recovery")
	errSyncJournalRecoveryRemoval    = errors.New("sync journal with unresolved destructive recovery state requires --force before removal")
	errSyncJournalNewerVersion       = errors.New("sync execution journal was written by a newer schema version")
	errSyncJournalUnsupportedVersion = errors.New("sync execution journal schema version is unsupported")
	errSyncJournalMigrationRequired  = errors.New("sync execution journal must be migrated before it can be written")
	errSyncJournalInvalidSchema      = syncjournalpkg.ErrInvalidSchema
)

type syncJournalPostcondition = syncjournalpkg.Postcondition
type syncJournalRunStats = syncjournalpkg.RunStats
type syncJournalMigrationRecord = syncjournalpkg.MigrationRecord
type syncJournalItem = syncjournalpkg.Item
type syncExecutionJournal = syncjournalpkg.Journal

type syncJournalLocation struct {
	Dir         string
	JournalPath string
	LockPath    string
	LeasePath   string
}

type syncJournalStore struct {
	Root             string
	ProfileScope     string
	AccountID        int64
	AutoGC           bool
	GCInterval       time.Duration
	Retention        time.Duration
	TrashRetention   time.Duration
	writePrivateFile func(string, []byte) error
}

type syncJournalEntry = syncjournalpkg.ListEntry

type syncJournalHandle struct {
	mu       sync.Mutex
	store    syncJournalStore
	location syncJournalLocation
	lock     *transfer.SessionLock
	journal  syncExecutionJournal
}

func resolveSyncJournalStore() (syncJournalStore, error) {
	config, err := auth.ResolveSessionStoreConfig(configPath)
	if err != nil {
		return syncJournalStore{}, err
	}
	profileName := auth.ResolveProfileName(configPath, profile)
	profileScope, err := transfer.SessionProfileScope(auth.ResolveConfigFilePath(configPath), profileName)
	if err != nil {
		return syncJournalStore{}, err
	}
	accountID := int64(0)
	if client != nil {
		accountID = client.UserID
	}
	return syncJournalStore{
		Root: config.SessionDir, ProfileScope: profileScope, AccountID: accountID,
		AutoGC: config.SessionAutoGC, GCInterval: config.SessionGCInterval,
		Retention: config.SessionRetention, TrashRetention: config.SessionTrashRetention,
	}, nil
}

func (store syncJournalStore) root() (string, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return "", errors.New("sync journal root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	scope := strings.ToLower(strings.TrimSpace(store.ProfileScope))
	if len(scope) != 64 {
		return "", fmt.Errorf("invalid sync journal profile scope")
	}
	if _, err := hex.DecodeString(scope); err != nil {
		return "", fmt.Errorf("invalid sync journal profile scope: %w", err)
	}
	return filepath.Join(absolute, "sync", syncJournalLayoutVersion, scope), nil
}

func (store syncJournalStore) location(planID string) (syncJournalLocation, error) {
	planID, err := normalizeSyncPlanID(planID)
	if err != nil || planID == "" {
		if err == nil {
			err = errors.New("sync journal plan ID is empty")
		}
		return syncJournalLocation{}, err
	}
	root, err := store.root()
	if err != nil {
		return syncJournalLocation{}, err
	}
	dir := filepath.Join(root, planID[:2], planID)
	lockRoot := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(root))), "sync-locks", strings.ToLower(store.ProfileScope), planID[:2])
	return syncJournalLocation{
		Dir: dir, JournalPath: filepath.Join(dir, "journal.json"), LeasePath: filepath.Join(dir, "lease.json"),
		LockPath: filepath.Join(lockRoot, planID+".lock"),
	}, nil
}

func newSyncExecutionJournal(plan syncPlan, profileScope string, accountID int64) (syncExecutionJournal, error) {
	planID, err := normalizeSyncPlanID(plan.PlanID)
	if err != nil || planID == "" {
		return syncExecutionJournal{}, fmt.Errorf("sync plan has invalid plan ID: %w", err)
	}
	plan.PlanID = planID
	return syncjournalpkg.New(plan, profileScope, accountID, time.Now().UTC())
}

func validateSyncJournalRunStats(stats syncJournalRunStats) error {
	return syncjournalpkg.ValidateRunStats(stats)
}

func restoreSyncJournalPlan(journal *syncExecutionJournal) error {
	if journal == nil {
		return errors.New("sync journal is nil")
	}
	if err := validateSyncJournalVersion(journal.Version); err != nil {
		return err
	}
	if journal.Version >= 2 && journal.Schema != syncJournalSchemaID {
		return fmt.Errorf("%w: schema v%d requires identity %q", errSyncJournalInvalidSchema, journal.Version, syncJournalSchemaID)
	}
	if err := syncjournalpkg.ValidateJournalState(journal.State); err != nil {
		return err
	}
	planID, err := normalizeSyncPlanID(journal.PlanID)
	if err != nil || planID == "" {
		return fmt.Errorf("invalid sync journal plan ID: %w", err)
	}
	if len(journal.Items) != len(journal.Plan.Items) {
		return errors.New("sync journal item count does not match stored plan")
	}
	for index := range journal.Plan.Items {
		if err := syncjournalpkg.RestoreStoredItem(index, journal.Items[index], &journal.Plan.Items[index]); err != nil {
			return err
		}
	}
	if fingerprint := syncPlanFingerprint(journal.Plan); fingerprint != planID {
		return fmt.Errorf("sync journal stored plan fingerprint changed: expected %s got %s", planID, fingerprint)
	}
	journal.PlanID = planID
	journal.Plan.PlanID = planID
	journal.MigrationRequired = journal.Version < syncJournalVersion
	journal.Status = syncJournalEffectiveStatus(*journal)
	return nil
}

func (store syncJournalStore) write(location syncJournalLocation, journal syncExecutionJournal) error {
	if journal.Version != syncJournalVersion {
		return fmt.Errorf("%w: have version %d, need version %d", errSyncJournalMigrationRequired, journal.Version, syncJournalVersion)
	}
	if journal.Schema != syncJournalSchemaID {
		return fmt.Errorf("%w: schema v%d requires identity %q", errSyncJournalInvalidSchema, journal.Version, syncJournalSchemaID)
	}
	journal.MigrationRequired = false
	journal.Status = syncJournalEffectiveStatus(journal)
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sync execution journal: %w", err)
	}
	if err := store.writePrivate(location.JournalPath, encoded); err != nil {
		return fmt.Errorf("write sync execution journal: %w", err)
	}
	return nil
}

func (store syncJournalStore) read(location syncJournalLocation) (syncExecutionJournal, error) {
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return syncExecutionJournal{}, errSyncJournalNotFound
		}
		return syncExecutionJournal{}, err
	}
	journal, err := decodeSyncJournalData(data)
	if err != nil {
		return syncExecutionJournal{}, err
	}
	if err := store.validateJournalBinding(journal); err != nil {
		return syncExecutionJournal{}, err
	}
	expectedPlanID := strings.ToLower(filepath.Base(location.Dir))
	if journal.PlanID != expectedPlanID {
		return syncExecutionJournal{}, fmt.Errorf("%w: journal plan_id %q does not match storage path %q", errSyncJournalInvalidSchema, journal.PlanID, expectedPlanID)
	}
	return journal, nil
}

func (store syncJournalStore) Create(plan syncPlan) (*syncJournalHandle, error) {
	journal, err := newSyncExecutionJournal(plan, store.ProfileScope, store.AccountID)
	if err != nil {
		return nil, err
	}
	if err := store.ensureJournalMutationAllowed(journal.PlanID); err != nil {
		return nil, err
	}
	location, err := store.location(journal.PlanID)
	if err != nil {
		return nil, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(location.JournalPath); err == nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %s; use --resume %s or remove the journal", errSyncJournalExists, journal.PlanID, journal.PlanID)
	} else if !os.IsNotExist(err) {
		_ = lock.Close()
		return nil, err
	}
	if err := store.write(location, journal); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &syncJournalHandle{store: store, location: location, lock: lock, journal: journal}, nil
}

func normalizeSyncJournalPrefix(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 8 || len(value) > 64 {
		return "", fmt.Errorf("sync journal ID prefix must contain 8 to 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("sync journal ID prefix must be hexadecimal")
	}
	return value, nil
}

func (store syncJournalStore) resolvePrefix(prefix string) (string, error) {
	prefix, err := normalizeSyncJournalPrefix(prefix)
	if err != nil {
		return "", err
	}
	ids, err := store.listPlanIDs()
	if err != nil {
		return "", err
	}
	matches := make([]string, 0, 1)
	for _, planID := range ids {
		if strings.HasPrefix(planID, prefix) {
			matches = append(matches, planID)
		}
	}
	if len(matches) == 0 {
		return "", errSyncJournalNotFound
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("sync journal ID prefix %q is ambiguous", prefix)
	}
	return matches[0], nil
}

func (store syncJournalStore) listPlanIDs() ([]string, error) {
	root, err := store.root()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
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
		planID := strings.ToLower(filepath.Base(filepath.Dir(current)))
		if len(planID) != 64 {
			return nil
		}
		if _, err := hex.DecodeString(planID); err != nil {
			return nil
		}
		ids = append(ids, planID)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

func (store syncJournalStore) Open(prefix string) (*syncJournalHandle, error) {
	planID, err := store.resolvePrefix(prefix)
	if err != nil {
		return nil, err
	}
	if err := store.ensureJournalMutationAllowed(planID); err != nil {
		return nil, err
	}
	location, err := store.location(planID)
	if err != nil {
		return nil, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return nil, err
	}
	journal, err := store.read(location)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if journal.Version < syncJournalVersion {
		journal, _, err = store.migrateLocationLocked(location)
		if err != nil {
			_ = lock.Close()
			return nil, err
		}
	}
	return &syncJournalHandle{store: store, location: location, lock: lock, journal: journal}, nil
}

func (handle *syncJournalHandle) Close() error {
	if handle == nil || handle.lock == nil {
		return nil
	}
	err := handle.lock.Close()
	handle.lock = nil
	return err
}

func cloneSyncExecutionJournal(journal syncExecutionJournal) syncExecutionJournal {
	return syncjournalpkg.Clone(journal)
}

func (handle *syncJournalHandle) snapshot() syncExecutionJournal {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	snapshot := cloneSyncExecutionJournal(handle.journal)
	snapshot.Status = syncJournalEffectiveStatus(snapshot)
	return snapshot
}

func (handle *syncJournalHandle) mutate(fn func(*syncExecutionJournal) error) error {
	if handle == nil {
		return errors.New("sync journal handle is nil")
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	next := cloneSyncExecutionJournal(handle.journal)
	if err := fn(&next); err != nil {
		return err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Status = syncJournalEffectiveStatus(next)
	if err := handle.store.write(handle.location, next); err != nil {
		return err
	}
	handle.journal = next
	return nil
}

func syncJournalRecoveryRequired(journal syncExecutionJournal) bool {
	return syncjournalpkg.RecoveryRequired(journal)
}

func syncJournalDestructiveReconciliationRequired(journal syncExecutionJournal) bool {
	return syncjournalpkg.DestructiveReconciliationRequired(journal)
}

func syncJournalEffectiveStatus(journal syncExecutionJournal) string {
	return syncjournalpkg.EffectiveStatus(journal)
}

func syncPlanItemDestructive(item syncPlanItem) bool {
	return syncjournalpkg.IsDestructivePlanItem(item)
}

func syncJournalCountKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unset"
	}
	return value
}

func formatSyncJournalCountMap(counts map[string]int) string {
	if len(counts) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func formatSyncJournalStaleAge(milliseconds int64) string {
	if milliseconds <= 0 {
		return "0s"
	}
	return (time.Duration(milliseconds) * time.Millisecond).Truncate(time.Second).String()
}

func (store syncJournalStore) List() ([]syncJournalEntry, error) {
	root, err := store.root()
	if err != nil {
		return nil, err
	}
	entries := make([]syncJournalEntry, 0)
	now := time.Now().UTC()
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
		location := syncJournalLocation{Dir: filepath.Dir(current), JournalPath: current}
		planID := filepath.Base(location.Dir)
		fullLocation, locErr := store.location(planID)
		if locErr != nil {
			return fmt.Errorf("inspect sync journal %q: %w", planID, locErr)
		}
		journal, readErr := store.read(fullLocation)
		if readErr != nil {
			return fmt.Errorf("inspect sync journal %q: %w", planID, readErr)
		}
		inUse, _ := transfer.SessionLockInUse(fullLocation.LockPath)
		item := syncjournalpkg.BuildListEntry(journal, now, inUse)
		entries = append(entries, item)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt.Equal(entries[j].UpdatedAt) {
			return entries[i].PlanID < entries[j].PlanID
		}
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
	return entries, nil
}

func (store syncJournalStore) Inspect(prefix string) (syncExecutionJournal, error) {
	planID, err := store.resolvePrefix(prefix)
	if err != nil {
		return syncExecutionJournal{}, err
	}
	location, err := store.location(planID)
	if err != nil {
		return syncExecutionJournal{}, err
	}
	return store.read(location)
}

func (store syncJournalStore) Trash(prefix string) (string, error) {
	return store.trash(prefix, false)
}

func (store syncJournalStore) ForceTrash(prefix string) (string, error) {
	return store.trash(prefix, true)
}

func (store syncJournalStore) trash(prefix string, force bool) (string, error) {
	planID, err := store.resolvePrefix(prefix)
	if err != nil {
		return "", err
	}
	if err := store.ensureJournalMutationAllowed(planID); err != nil {
		return "", err
	}
	trash, err := store.sharedCurrentStore().TrashCurrent(planID, force, time.Now().UTC())
	if errors.Is(err, syncjournalpkg.ErrMigrationRequired) {
		// Historical contract: rm/gc may soft-delete a readable legacy journal
		// without migrating or rewriting it first. Current-v2 journals use the
		// shared alias-aware path above; only an explicit legacy-schema result
		// falls back to the original raw-lock + rename behavior.
		location, locationErr := store.location(planID)
		if locationErr != nil {
			return "", locationErr
		}
		lock, lockErr := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
		if lockErr != nil {
			return "", lockErr
		}
		defer lock.Close()
		journal, readErr := store.read(location)
		if readErr != nil {
			return "", readErr
		}
		if !force && (syncJournalRecoveryRequired(journal) || syncJournalDestructiveReconciliationRequired(journal)) {
			return "", errSyncJournalRecoveryRemoval
		}
		if stopErr := lock.StopLease(); stopErr != nil {
			return "", stopErr
		}
		return syncjournalpkg.MoveDirectoryToSessionTrash(store.Root, location.Dir, journal.PlanID, time.Now().UTC())
	}
	if errors.Is(err, syncjournalpkg.ErrTrashRecoveryRemoval) {
		return "", errSyncJournalRecoveryRemoval
	}
	return trash, err
}

type syncJournalGCAction struct {
	PlanID string `json:"plan_id"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func (store syncJournalStore) GC(olderThan time.Duration, dryRun bool) ([]syncJournalGCAction, error) {
	olderThan = syncjournalpkg.ResolveGCRetention(olderThan, store.Retention)
	entries, err := store.List()
	if err != nil {
		return nil, err
	}
	protected, err := store.migrationBatchProtectedPlanIDs()
	if err != nil {
		return nil, err
	}
	candidates := syncjournalpkg.SelectGCCandidates(entries, time.Now().UTC(), olderThan, protected)
	actions := make([]syncJournalGCAction, 0, len(candidates))
	for _, candidate := range candidates {
		action := syncJournalGCAction{PlanID: candidate.PlanID, Action: "trash", Reason: candidate.State}
		if !dryRun {
			if _, err := store.Trash(candidate.PlanID); err != nil {
				return actions, err
			}
		}
		actions = append(actions, action)
	}
	return actions, nil
}

var (
	syncJournalListState   string
	syncJournalGCDryRun    bool
	syncJournalGCOlderThan string
	syncJournalRmForce     bool
)

var syncJournalCmd = &cobra.Command{
	Use:   "journal",
	Short: "Inspect and maintain persisted sync execution journals",
	Args:  cobra.NoArgs,
	Long:  "Inspect and maintain persisted sync execution journals. Read-only list/inspect/verify accept legacy readable schemas without rewriting them. Writable Open paths such as resume/recover atomically migrate legacy journals under the journal lock before any mutation; 'sync journal migrate' performs that upgrade explicitly without executing sync data actions. Journals from newer schema versions fail closed and are never downgraded.",
}

func normalizeSyncJournalListState(value string) (string, error) {
	state := strings.ToLower(strings.TrimSpace(value))
	if state != "" && state != syncJournalStatusActive && state != syncJournalStatusFailed && state != syncJournalStatusCompleted && state != syncJournalStatusReconcileRequired && state != syncJournalStatusRecoveryRequired {
		return "", fmt.Errorf("invalid --state %q", value)
	}
	return state, nil
}

func syncJournalListArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, err := normalizeSyncJournalListState(syncJournalListState); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return nil
}

func syncJournalGCArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if strings.TrimSpace(syncJournalGCOlderThan) != "" {
		if _, err := parseSessionAge(syncJournalGCOlderThan); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	return nil
}

var syncJournalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sync execution journals with state/action/phase counters",
	Args:  syncJournalListArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		entries, err := store.List()
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}
		state, err := normalizeSyncJournalListState(syncJournalListState)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		filtered := make([]syncJournalEntry, 0, len(entries))
		for _, entry := range entries {
			if state == "" || entry.Status == state {
				filtered = append(filtered, entry)
			}
		}
		printer.PrintSuccess(filtered)
		if !jsonOutput {
			for _, entry := range filtered {
				stateText := entry.Status
				flags := ""
				if entry.MigrationRequired {
					flags += " migration-required"
				}
				if entry.InUse {
					flags += " in-use"
				}
				fmt.Printf("%s  v%d %-18s items=%d done=%d pending=%d failed=%d blocked=%d stale=%s  %s <-> %s%s\n", entry.PlanID[:12], entry.Version, stateText, entry.Total, entry.Completed, entry.Pending, entry.Failed, entry.Blocked, formatSyncJournalStaleAge(entry.StaleForMillis), entry.LocalRoot, entry.RemoteRoot, flags)
				fmt.Printf("  actions=%s states=%s phases=%s\n", formatSyncJournalCountMap(entry.ActionCounts), formatSyncJournalCountMap(entry.StateCounts), formatSyncJournalCountMap(entry.PhaseCounts))
			}
		}
		return nil
	},
}

var syncJournalInspectCmd = &cobra.Command{
	Use:   "inspect <plan_id>",
	Short: "Inspect one persisted sync execution journal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		journal, err := store.Inspect(args[0])
		if err != nil {
			return syncJournalExitError(err)
		}
		printer.PrintSuccess(journal)
		if !jsonOutput {
			encoded, _ := json.MarshalIndent(journal, "", "  ")
			fmt.Println(string(encoded))
		}
		return nil
	},
}

var syncJournalRmCmd = &cobra.Command{
	Use:   "rm <plan_id>",
	Short: "Move one sync execution journal to session trash",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		var path string
		if syncJournalRmForce {
			path, err = store.ForceTrash(args[0])
		} else {
			path, err = store.Trash(args[0])
		}
		if err != nil {
			return syncJournalExitError(err)
		}
		data := map[string]string{"trash_path": path}
		printer.PrintSuccess(data)
		if !jsonOutput {
			fmt.Printf("Sync journal moved to trash: %s\n", path)
		}
		return nil
	},
}

var syncJournalGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Trash old completed or failed sync journals",
	Args:  syncJournalGCArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		olderThan := time.Duration(0)
		if strings.TrimSpace(syncJournalGCOlderThan) != "" {
			olderThan, err = parseSessionAge(syncJournalGCOlderThan)
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error()}
			}
		}
		actions, err := store.RunGCExclusive(olderThan, syncJournalGCDryRun)
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}
		printer.PrintSuccess(actions)
		if !jsonOutput {
			for _, action := range actions {
				fmt.Printf("%-6s %-12s %s\n", action.Action, action.Reason, action.PlanID)
			}
			if len(actions) == 0 {
				fmt.Println("No sync journal maintenance needed.")
			}
		}
		return nil
	},
}

func syncJournalExitCode(err error) int {
	code := classifyRemoteError(err, output.ExitError)
	if errors.Is(err, errSyncJournalNotFound) || errors.Is(err, syncjournalpkg.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		code = output.ExitNotFound
	}
	if errors.Is(err, transfer.ErrSessionLocked) || errors.Is(err, errSyncJournalCompleted) || errors.Is(err, errSyncJournalRecoveryRequired) || errors.Is(err, errSyncJournalRecoveryRemoval) || errors.Is(err, errSyncJournalNewerVersion) || errors.Is(err, errSyncJournalUnsupportedVersion) || errors.Is(err, errSyncJournalMigrationRequired) || errors.Is(err, errSyncJournalInvalidSchema) || errors.Is(err, errSyncJournalMigrationBatchNotFound) || errors.Is(err, errSyncJournalMigrationBatchExists) || errors.Is(err, errSyncJournalMigrationBatchRecoveryRequired) || errors.Is(err, errSyncJournalAliasNotOrphan) || errors.Is(err, errSyncJournalAliasRepairChanged) || errors.Is(err, syncjournalpkg.ErrReviewAliasChanged) || errors.Is(err, syncjournalpkg.ErrReviewAliasTrashed) || errors.Is(err, syncjournalpkg.ErrReviewAliasConflict) || errors.Is(err, syncjournalpkg.ErrReviewAliasInUse) || errors.Is(err, syncjournalpkg.ErrReviewAliasRepairRollback) || errors.Is(err, syncjournalpkg.ErrTrashScanLimit) || errors.Is(err, syncjournalpkg.ErrTrashEntryChanged) || errors.Is(err, syncjournalpkg.ErrRestoreCurrentExists) || errors.Is(err, syncjournalpkg.ErrCleanupMigrationInProgress) {
		code = output.ExitArgs
	}
	return code
}

func syncJournalExitError(err error) error {
	return &exitError{code: syncJournalExitCode(err), msg: err.Error()}
}

func syncJournalExitErrorData(err error, data interface{}) error {
	return &exitError{code: syncJournalExitCode(err), msg: err.Error(), data: data}
}

func init() {
	syncJournalListCmd.Flags().StringVar(&syncJournalListState, "state", "", "Filter by active, failed, completed, reconcile-required, or recovery-required state")
	syncJournalMigrateCmd.Flags().BoolVar(&syncJournalMigrateAll, "all", false, "Migrate every legacy journal after a store-wide read-only doctor preflight")
	syncJournalMigrateCmd.Flags().BoolVar(&syncJournalMigrateRecoverBatch, "recover-batch", false, "Reconcile an interrupted bulk migration from exact source/target hashes and verified backups")
	syncJournalRmCmd.Flags().BoolVar(&syncJournalRmForce, "force", false, "Allow removing a recovery-required journal after manual review")
	syncJournalGCCmd.Flags().BoolVar(&syncJournalGCDryRun, "dry-run", false, "Show journals that would be trashed without changing data")
	syncJournalGCCmd.Flags().StringVar(&syncJournalGCOlderThan, "older-than", "", "Only collect completed/failed journals older than this duration (default transfer.sessions.retention)")
	syncJournalCmd.AddCommand(syncJournalSchemaCmd, syncJournalListCmd, syncJournalInspectCmd, syncJournalDoctorCmd, syncJournalVerifyCmd, syncJournalRecoverCmd, syncJournalMigrateCmd, syncJournalRmCmd, syncJournalGCCmd)
	syncCmd.AddCommand(syncJournalCmd)
}
