package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type mcpSyncJournalRun struct {
	handle          *syncjournalpkg.Handle
	resumed         bool
	completedBefore int
	indexByKey      map[string]int
	startedAt       time.Time
}

var errMCPSyncJournalMutationUnverified = errors.New("sync journal completed mutation is not yet verifiable")

func (ft *FileTools) resolveSyncJournalStore(ctx context.Context) (*syncjournalpkg.Store, error) {
	if ft == nil || ft.syncJournalStore == nil {
		return nil, nil
	}
	store := *ft.syncJournalStore
	if store.AccountID > 0 {
		return &store, nil
	}
	if ft.client == nil {
		return nil, errors.New("sync journal account client is nil")
	}
	accountID := ft.client.UserID
	if accountID <= 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		user, err := ft.client.GetUser()
		if err != nil {
			return nil, fmt.Errorf("resolve sync journal account identity: %w", err)
		}
		if user == nil || user.UserID <= 0 {
			return nil, errors.New("resolve sync journal account identity returned an invalid user")
		}
		accountID = user.UserID
	}
	store.AccountID = accountID
	return &store, nil
}

func countMCPJournalCompleted(journal syncjournalpkg.Journal) int {
	completed := 0
	for _, item := range journal.Items {
		if item.State == "succeeded" || item.State == "skipped" {
			completed++
		}
	}
	return completed
}

func safeMCPJournalRetry(journal syncjournalpkg.Journal) bool {
	if syncjournalpkg.RecoveryRequired(journal) || syncjournalpkg.ReconciliationRequired(journal) || journal.State == syncjournalpkg.StatusCompleted {
		return false
	}
	for _, item := range journal.Items {
		if item.State == "succeeded" || (item.State != "succeeded" && item.State != "skipped" && item.Phase == syncjournalpkg.PhaseMutationDone) {
			return false
		}
	}
	return journal.State == syncjournalpkg.StatusActive || journal.State == syncjournalpkg.StatusFailed
}

func resetMCPJournalForRetry(handle *syncjournalpkg.Handle) error {
	return handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		if !safeMCPJournalRetry(*journal) {
			return errors.New("sync journal is not safe to retry")
		}
		journal.State = syncjournalpkg.StatusActive
		journal.Status = syncjournalpkg.StatusActive
		journal.LastError = ""
		journal.CompletedAt = nil
		for index := range journal.Items {
			stored := &journal.Items[index]
			if journal.Plan.Items[index].Action == "skip" {
				stored.State = "skipped"
				stored.Phase = syncjournalpkg.PhaseDone
				continue
			}
			stored.State = "pending"
			stored.Phase = syncjournalpkg.PhasePending
			stored.LastError = ""
			stored.Post = nil
		}
		return nil
	})
}

func sameMCPSyncJournalLocalRoot(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func validateMCPSyncJournalRequestBinding(ft *FileTools, args PlanSyncArgs, journal syncjournalpkg.Journal) (mcpSyncPlanNormalizedArgs, error) {
	if ft == nil {
		return mcpSyncPlanNormalizedArgs{}, errors.New("sync journal file tools are unavailable")
	}
	normalized, err := normalizeMCPSyncPlanArgs(ft.localRoot, args)
	if err != nil {
		return mcpSyncPlanNormalizedArgs{}, err
	}
	if !sameMCPSyncJournalLocalRoot(normalized.localPath, journal.Plan.LocalRoot) {
		return mcpSyncPlanNormalizedArgs{}, errors.New("sync journal local root does not match the reviewed request")
	}
	if normalized.remotePath != syncplanpkg.CanonicalRemoteRoot(journal.Plan.RemoteRoot) {
		return mcpSyncPlanNormalizedArgs{}, errors.New("sync journal remote root does not match the reviewed request")
	}
	if normalized.options.Direction != journal.Plan.Direction || normalized.options.ConflictPolicy != journal.Plan.ConflictPolicy || normalized.options.DeleteExtraneous != journal.Plan.DeleteExtraneous {
		return mcpSyncPlanNormalizedArgs{}, errors.New("sync journal policy does not match the reviewed request")
	}
	return normalized, nil
}

func validateMCPSyncJournalCompletedPostconditions(ctx context.Context, ft *FileTools, journal syncjournalpkg.Journal, maxChecksumBytes int64) error {
	checksumBytes := int64(0)
	for _, stored := range journal.Items {
		if stored.State != "succeeded" || stored.Post == nil || stored.Post.Side != "local" || !stored.Post.Exists || stored.Post.Kind != "file" {
			continue
		}
		if stored.Post.Size < 0 || checksumBytes > maxChecksumBytes-stored.Post.Size {
			return errors.New("sync journal completed local checksum budget exceeded")
		}
		checksumBytes += stored.Post.Size
	}
	executor := &mcpSyncExecutor{ft: ft, plan: journal.Plan}
	for index, stored := range journal.Items {
		if stored.State != "succeeded" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stored.Post == nil {
			return fmt.Errorf("completed sync journal item %q has no postcondition", journal.Plan.Items[index].RelativePath)
		}
		actual, err := executor.captureJournalPostcondition(ctx, journal.Plan.Items[index])
		if err != nil {
			return fmt.Errorf("validate completed sync journal item %q: %w", journal.Plan.Items[index].RelativePath, err)
		}
		if !syncjournalpkg.PostconditionEqual(stored.Post, actual) {
			return fmt.Errorf("completed sync journal item %q no longer satisfies its recorded postcondition", journal.Plan.Items[index].RelativePath)
		}
	}
	return nil
}

func preflightMCPSyncJournalResume(ctx context.Context, ft *FileTools, args PlanSyncArgs, journal syncjournalpkg.Journal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := validateMCPSyncJournalRequestBinding(ft, args, journal)
	if err != nil {
		return err
	}
	if syncjournalpkg.RecoveryRequired(journal) || syncjournalpkg.DestructiveReconciliationRequired(journal) {
		return errors.New("sync journal requires destructive recovery review")
	}
	if syncjournalpkg.PostconditionVerificationRequired(journal) {
		return errMCPSyncJournalMutationUnverified
	}
	expected, err := syncjournalpkg.ExpectedPlan(journal)
	if err != nil {
		return err
	}
	current, err := scanMCPSyncTrees(ctx, ft.client, ft.localRoot, args)
	if err != nil {
		return fmt.Errorf("rescan sync trees for journal resume: %w", err)
	}
	if !sameMCPSyncJournalLocalRoot(current.localSnapshot.Root, expected.LocalRoot) || current.normalized.remotePath != expected.RemoteRoot || current.remoteRootID != expected.RemoteRootID {
		return errors.New("sync roots changed identity after journal execution")
	}
	syncjournalpkg.RemoveInterruptedDownloadArtifacts(current.localSnapshot.Entries, journal)
	if err := syncjournalpkg.CompareExpectedLocalTree(expected, current.localSnapshot.Entries); err != nil {
		return err
	}
	if err := syncjournalpkg.CompareExpectedRemoteTree(expected, current.remoteEntries, func(entry syncplanpkg.Entry) (string, error) {
		return resolveMCPSyncRemoteSHA1(ft.client, entry)
	}); err != nil {
		return err
	}
	if err := validateMCPSyncJournalCompletedPostconditions(ctx, ft, journal, normalized.maxChecksumBytes); err != nil {
		return err
	}
	return ctx.Err()
}

func resetMCPJournalForResidualResume(handle *syncjournalpkg.Handle) error {
	if handle == nil {
		return errors.New("sync journal handle is nil")
	}
	return handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		if syncjournalpkg.RecoveryRequired(*journal) || syncjournalpkg.DestructiveReconciliationRequired(*journal) {
			return errors.New("sync journal requires destructive recovery review")
		}
		if journal.State == syncjournalpkg.StatusCompleted {
			return errors.New("sync journal is already completed")
		}
		if syncjournalpkg.PostconditionVerificationRequired(*journal) {
			return errMCPSyncJournalMutationUnverified
		}
		for index := range journal.Items {
			stored := &journal.Items[index]
			planned := journal.Plan.Items[index]
			if stored.State == "succeeded" || stored.State == "skipped" {
				continue
			}
			if planned.Action == "skip" {
				stored.State = "skipped"
				stored.Phase = syncjournalpkg.PhaseDone
				stored.LastError = ""
				stored.Post = nil
				continue
			}
			stored.State = "pending"
			stored.Phase = syncjournalpkg.PhasePending
			stored.LastError = ""
			stored.Post = nil
		}
		journal.State = syncjournalpkg.StatusActive
		journal.LastError = ""
		journal.CompletedAt = nil
		return nil
	})
}

func beginMCPSyncJournalRun(handle *syncjournalpkg.Handle, resumed bool) (*mcpSyncJournalRun, error) {
	if handle == nil {
		return nil, errors.New("sync journal handle is nil")
	}
	snapshot := handle.Snapshot()
	indexByKey := make(map[string]int, len(snapshot.Plan.Items))
	for index, item := range snapshot.Plan.Items {
		key := syncplanpkg.PathKey(item.RelativePath)
		if _, exists := indexByKey[key]; exists {
			return nil, fmt.Errorf("sync journal contains duplicate relative path %q", item.RelativePath)
		}
		indexByKey[key] = index
	}
	run := &mcpSyncJournalRun{
		handle: handle, resumed: resumed, completedBefore: countMCPJournalCompleted(snapshot),
		indexByKey: indexByKey, startedAt: time.Now().UTC(),
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		stats := &journal.RunStats
		if stats.LastStartedAt != nil && (stats.LastFinishedAt == nil || stats.LastFinishedAt.Before(*stats.LastStartedAt)) {
			stats.InterruptedRuns++
		}
		stats.Runs++
		if resumed {
			stats.ResumeRuns++
		}
		started := run.startedAt
		stats.LastStartedAt = &started
		stats.LastFinishedAt = nil
		stats.LastDurationMillis = 0
		journal.State = syncjournalpkg.StatusActive
		journal.LastError = ""
		journal.CompletedAt = nil
		return nil
	}); err != nil {
		return nil, err
	}
	return run, nil
}

// prepareMCPSyncResidualResume resolves an existing private review alias and,
// when present, opens the original current-v2 journal under the shared lock.
// It never treats a changed fresh MCP plan ID as permission to replay: the
// stored request binding, original destructive budget, expected whole-tree
// state, and every completed postcondition are revalidated first.
func (ft *FileTools) prepareMCPSyncResidualResume(ctx context.Context, reviewedPlanID string, args PlanSyncArgs, deleteBudget mcpSyncDeleteBudget) (*mcpSyncJournalRun, syncplanpkg.Plan, bool, string, string, error) {
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil {
		return nil, syncplanpkg.Plan{}, true, "journal_unavailable", "persistent sync journal store is unavailable", nil
	}
	if store == nil {
		return nil, syncplanpkg.Plan{}, false, "", "", nil
	}
	rawPlanID, aliasErr := store.ResolveReviewAlias(reviewedPlanID)
	if aliasErr != nil && !errors.Is(aliasErr, syncjournalpkg.ErrNotFound) {
		return nil, syncplanpkg.Plan{}, true, "journal_read_failed", "persistent sync journal alias could not be read safely", nil
	}
	aliasBackfill := errors.Is(aliasErr, syncjournalpkg.ErrNotFound)
	if aliasBackfill {
		rawPlanID = ""
	}
	var handle *syncjournalpkg.Handle
	for {
		if rawPlanID == "" {
			lookup := ft.lookupMCPSyncJournal(ctx, reviewedPlanID)
			if !lookup.found() {
				if lookup.ErrorCode == "journal_not_found" {
					return nil, syncplanpkg.Plan{}, false, "", "", nil
				}
				return nil, syncplanpkg.Plan{}, true, lookup.ErrorCode, lookup.Error, nil
			}
			rawPlanID = lookup.Record.Journal.PlanID
			aliasBackfill = true
		}

		handle, err = store.OpenCurrent(rawPlanID)
		if errors.Is(err, transfer.ErrSessionLocked) {
			return nil, syncplanpkg.Plan{}, true, "journal_in_use", "the reviewed sync plan is already executing in another process", nil
		}
		if errors.Is(err, syncjournalpkg.ErrNotFound) {
			if aliasBackfill {
				// The scan candidate disappeared before its raw lock was acquired;
				// with no trusted alias there is no persistent resume state left.
				return nil, syncplanpkg.Plan{}, false, "", "", nil
			}
			// Historical/crash leftovers may leave a private review alias after its
			// raw current journal has disappeared. Heal only after proving absence
			// under the raw-plan lock and alias lock, then attempt one bounded
			// current-v2 projection scan before falling back to fresh execution.
			_, healErr := store.RemoveOrphanReviewAlias(reviewedPlanID, rawPlanID)
			if healErr != nil {
				switch {
				case errors.Is(healErr, transfer.ErrSessionLocked), errors.Is(healErr, syncjournalpkg.ErrReviewAliasInUse):
					return nil, syncplanpkg.Plan{}, true, "journal_in_use", "the reviewed sync journal binding is being updated by another process", nil
				case errors.Is(healErr, syncjournalpkg.ErrReviewAliasTrashed):
					return nil, syncplanpkg.Plan{}, true, "journal_trashed", "the reviewed sync journal is soft-deleted; restore it from Session Store trash before execution", nil
				case errors.Is(healErr, syncjournalpkg.ErrReviewAliasConflict):
					return nil, syncplanpkg.Plan{}, true, "journal_alias_conflict", "the reviewed sync journal binding changed while stale state was being repaired", nil
				default:
					return nil, syncplanpkg.Plan{}, true, "journal_read_failed", "stale sync journal binding could not be repaired safely", nil
				}
			}
			rawPlanID = ""
			aliasBackfill = true
			continue
		}
		if err != nil {
			switch {
			case errors.Is(err, syncjournalpkg.ErrMigrationRequired):
				return nil, syncplanpkg.Plan{}, true, "journal_migration_required", "the sync journal uses an older schema; migrate it with the 115driver CLI before execution", nil
			default:
				return nil, syncplanpkg.Plan{}, true, "journal_read_failed", "persistent sync journal could not be opened safely", nil
			}
		}
		if aliasBackfill {
			snapshot := handle.Snapshot()
			envelope, envelopeErr := buildMCPSyncPlanEnvelope(snapshot.Plan)
			if envelopeErr != nil || envelope.PlanID != reviewedPlanID {
				_ = handle.Close()
				return nil, syncplanpkg.Plan{}, true, "journal_state_changed", "persistent sync journal no longer matches the reviewed plan identity", nil
			}
			if _, bindErr := handle.BindReviewAlias(reviewedPlanID); bindErr != nil {
				_ = handle.Close()
				switch {
				case errors.Is(bindErr, syncjournalpkg.ErrReviewAliasInUse), errors.Is(bindErr, transfer.ErrSessionLocked):
					return nil, syncplanpkg.Plan{}, true, "journal_in_use", "the reviewed sync journal binding is being updated by another process", nil
				case errors.Is(bindErr, syncjournalpkg.ErrReviewAliasConflict):
					return nil, syncplanpkg.Plan{}, true, "journal_alias_conflict", "the reviewed plan is already bound to a different persistent sync journal", nil
				default:
					return nil, syncplanpkg.Plan{}, true, "journal_read_failed", "persistent sync journal alias could not be rebuilt safely", nil
				}
			}
		}
		break
	}
	closeWith := func(code, message string) (*mcpSyncJournalRun, syncplanpkg.Plan, bool, string, string, error) {
		_ = handle.Close()
		return nil, syncplanpkg.Plan{}, true, code, message, nil
	}
	snapshot := handle.Snapshot()
	envelope, envelopeErr := buildMCPSyncPlanEnvelope(snapshot.Plan)
	if envelopeErr != nil {
		return closeWith("journal_read_failed", "persistent sync journal plan identity could not be verified safely")
	}
	if envelope.PlanID != reviewedPlanID {
		return closeWith("journal_alias_conflict", "the reviewed sync journal binding does not match the journal's content-addressed plan identity")
	}
	if snapshot.Status == syncjournalpkg.StatusRecoveryRequired || syncjournalpkg.DestructiveReconciliationRequired(snapshot) {
		return closeWith("recovery_required", "a previous destructive sync attempt requires state review before any retry")
	}
	if syncjournalpkg.PostconditionVerificationRequired(snapshot) {
		return closeWith("journal_reconcile_required", "a previous sync mutation completed but its postcondition is not yet verified; diagnose_sync_recovery must observe it before any replay")
	}
	if snapshot.State == syncjournalpkg.StatusCompleted {
		return closeWith("journal_completed", "this reviewed sync plan already has a completed execution journal")
	}
	if snapshot.State != syncjournalpkg.StatusActive && snapshot.State != syncjournalpkg.StatusFailed {
		return closeWith("journal_resume_invalid", "the persistent sync journal is not in a resumable state")
	}
	if _, err := validateMCPSyncJournalRequestBinding(ft, args, snapshot); err != nil {
		return closeWith("journal_request_mismatch", "the reviewed sync journal does not match the requested roots or sync policy")
	}
	if err := deleteBudget.validate(snapshot.Plan); err != nil {
		return closeWith("delete_budget_exceeded", "the original reviewed sync plan exceeds the requested mirror-delete execution budget")
	}
	if err := preflightMCPSyncJournalResume(ctx, ft, args, snapshot); err != nil {
		if errors.Is(err, errMCPSyncJournalMutationUnverified) {
			return closeWith("journal_reconcile_required", "a previous sync mutation completed but its postcondition is not yet verifiable; do not replay it")
		}
		return closeWith("journal_state_changed", "persistent sync journal state no longer matches current local/remote trees; run plan_sync again")
	}
	if err := resetMCPJournalForResidualResume(handle); err != nil {
		if errors.Is(err, errMCPSyncJournalMutationUnverified) {
			return closeWith("journal_reconcile_required", "a previous sync mutation completed but its postcondition is not yet verifiable; do not replay it")
		}
		return closeWith("journal_resume_invalid", "the persistent sync journal cannot be resumed safely")
	}
	residual := syncjournalpkg.ResidualPlan(handle.Snapshot())
	if err := validateMCPSyncExecutablePlan(residual); err != nil {
		return closeWith("journal_resume_invalid", "the persistent sync journal residual plan is not executable safely")
	}
	run, err := beginMCPSyncJournalRun(handle, true)
	if err != nil {
		_ = handle.Close()
		return nil, syncplanpkg.Plan{}, true, "", "", fmt.Errorf("start resumed sync journal run: %w", err)
	}
	return run, residual, true, "", "", nil
}

// prepareMCPSyncJournal acquires the same cross-process lock/layout used by CLI
// sync journals. Existing same-plan journals are reusable only when a fresh
// replan still matches and the stored state never crossed a destructive phase.
func (ft *FileTools) prepareMCPSyncJournal(ctx context.Context, reviewedPlanID string, plan syncplanpkg.Plan) (*mcpSyncJournalRun, string, string, error) {
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return nil, "", "", err
	}
	handle, err := store.CreateCurrent(plan)
	resumed := false
	if err != nil {
		switch {
		case errors.Is(err, transfer.ErrSessionLocked):
			return nil, "journal_in_use", "the reviewed sync plan is already executing in another process", nil
		case errors.Is(err, syncjournalpkg.ErrExists):
			handle, err = store.OpenCurrent(plan.PlanID)
			if errors.Is(err, transfer.ErrSessionLocked) {
				return nil, "journal_in_use", "the reviewed sync plan is already executing in another process", nil
			}
			if err != nil {
				break
			}
			snapshot := handle.Snapshot()
			if snapshot.Status == syncjournalpkg.StatusRecoveryRequired || syncjournalpkg.DestructiveReconciliationRequired(snapshot) {
				_ = handle.Close()
				return nil, "recovery_required", "a previous destructive sync attempt requires state review before any retry", nil
			}
			if syncjournalpkg.PostconditionVerificationRequired(snapshot) {
				_ = handle.Close()
				return nil, "journal_reconcile_required", "a previous sync mutation completed but its postcondition is not yet verified; diagnose_sync_recovery must observe it before any replay", nil
			}
			if snapshot.State == syncjournalpkg.StatusCompleted {
				_ = handle.Close()
				return nil, "journal_completed", "this exact sync plan already has a completed execution journal", nil
			}
			if !safeMCPJournalRetry(snapshot) {
				_ = handle.Close()
				return nil, "recovery_required", "a previous sync attempt cannot be safely replayed; run plan_sync again before any retry", nil
			}
			if err := resetMCPJournalForRetry(handle); err != nil {
				_ = handle.Close()
				return nil, "", "", err
			}
			resumed = true
		default:
		}
	}
	if err != nil {
		if errors.Is(err, syncjournalpkg.ErrMigrationRequired) {
			return nil, "journal_migration_required", "the sync journal uses an older schema; migrate it with the 115driver CLI before execution", nil
		}
		return nil, "", "", err
	}
	if handle == nil {
		return nil, "", "", errors.New("sync journal handle is nil")
	}
	if _, aliasErr := handle.BindReviewAlias(reviewedPlanID); aliasErr != nil {
		_ = handle.Close()
		switch {
		case errors.Is(aliasErr, syncjournalpkg.ErrReviewAliasInUse):
			return nil, "journal_in_use", "the reviewed sync journal binding is being prepared by another process", nil
		case errors.Is(aliasErr, syncjournalpkg.ErrReviewAliasConflict):
			return nil, "journal_alias_conflict", "the reviewed plan is already bound to a different persistent sync journal", nil
		default:
			return nil, "", "", fmt.Errorf("persist reviewed sync journal binding: %w", aliasErr)
		}
	}

	run, err := beginMCPSyncJournalRun(handle, resumed)
	if err != nil {
		_ = handle.Close()
		return nil, "", "", err
	}
	return run, "", "", nil
}

func (run *mcpSyncJournalRun) close() error {
	if run == nil || run.handle == nil {
		return nil
	}
	return run.handle.Close()
}

func (run *mcpSyncJournalRun) itemIndex(item syncplanpkg.Item) (int, error) {
	if run == nil {
		return -1, errors.New("sync journal run is nil")
	}
	index, ok := run.indexByKey[syncplanpkg.PathKey(item.RelativePath)]
	if !ok {
		return -1, fmt.Errorf("sync journal has no item for %q", item.RelativePath)
	}
	return index, nil
}

func (run *mcpSyncJournalRun) setItemPhase(item syncplanpkg.Item, phase string) error {
	index, err := run.itemIndex(item)
	if err != nil {
		return err
	}
	return run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		return syncjournalpkg.SetItemPhase(journal, index, phase, time.Now().UTC())
	})
}

func (run *mcpSyncJournalRun) beforeItem(_ context.Context, index int, item syncplanpkg.Item) error {
	return run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		return syncjournalpkg.BeginItem(journal, index, item, time.Now().UTC())
	})
}

func (executor *mcpSyncExecutor) captureJournalPostcondition(ctx context.Context, item syncplanpkg.Item) (*syncjournalpkg.Postcondition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch item.Action {
	case "upload", "replace-remote":
		id, isDir, err := executor.resolver().ResolvePath(item.RemotePath)
		if err != nil {
			return nil, fmt.Errorf("capture remote sync postcondition: %w", err)
		}
		post := &syncjournalpkg.Postcondition{Side: "remote", Exists: true, RemoteID: id}
		if isDir {
			post.Kind = "directory"
			if item.Kind != post.Kind {
				return nil, errors.New("completed remote target has unexpected type")
			}
			return post, nil
		}
		file, err := executor.ft.client.GetFile(id)
		if err != nil {
			return nil, fmt.Errorf("capture remote sync file postcondition: %w", err)
		}
		if file == nil || file.IsDirectory {
			return nil, errors.New("capture remote sync file postcondition returned an unexpected object")
		}
		post.Kind = "file"
		post.Size = file.Size
		post.SHA1 = strings.ToUpper(strings.TrimSpace(file.Sha1))
		if !file.UpdateTime.IsZero() {
			post.ModTimeUnixNano = file.UpdateTime.UnixNano()
		}
		return post, nil
	case "download", "replace-local":
		localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, true)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(localPath)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("completed local sync target is a symlink")
		}
		post := &syncjournalpkg.Postcondition{Side: "local", Exists: true}
		if info.IsDir() {
			post.Kind = "directory"
			if item.Kind != post.Kind {
				return nil, errors.New("completed local target has unexpected type")
			}
			return post, nil
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("completed local sync target is not a regular file")
		}
		file, err := os.Open(localPath)
		if err != nil {
			return nil, err
		}
		digest, digestErr := uploadpkg.PrepareFileDigest(file, info.Size())
		_ = file.Close()
		if digestErr != nil {
			return nil, digestErr
		}
		post.Kind = "file"
		post.Size = info.Size()
		post.SHA1 = strings.ToUpper(strings.TrimSpace(digest.SHA1))
		post.ModTimeUnixNano = info.ModTime().UnixNano()
		return post, nil
	case "delete-remote":
		_, _, err := executor.resolver().ResolvePath(item.RemotePath)
		if err == nil {
			return nil, errors.New("remote delete target still exists after execution")
		}
		if !errors.Is(err, driver.ErrNotExist) {
			return nil, fmt.Errorf("verify remote delete postcondition: %w", err)
		}
		return &syncjournalpkg.Postcondition{Side: "remote", Exists: false}, nil
	case "delete-local":
		localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, false)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(localPath); err == nil {
			return nil, errors.New("local delete target still exists after execution")
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("verify local delete postcondition: %w", err)
		}
		return &syncjournalpkg.Postcondition{Side: "local", Exists: false}, nil
	case "skip":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported sync journal action %q", item.Action)
	}
}

func (run *mcpSyncJournalRun) afterItem(ctx context.Context, index int, item syncplanpkg.Item, outcome syncexecpkg.Outcome, executor *mcpSyncExecutor) error {
	if index < 0 || index >= len(run.handle.Snapshot().Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	if outcome.Err != nil {
		return run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
			return syncjournalpkg.FailItem(journal, index, outcome.Err.Error(), time.Now().UTC())
		})
	}
	if item.Action == "skip" {
		return run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
			return syncjournalpkg.SucceedItem(journal, index, item, nil, time.Now().UTC())
		})
	}
	post, err := executor.captureJournalPostcondition(ctx, item)
	if err != nil {
		message := "capture postcondition: " + err.Error()
		_ = run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
			return syncjournalpkg.FailItem(journal, index, message, time.Now().UTC())
		})
		return err
	}
	return run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		return syncjournalpkg.SucceedItem(journal, index, item, post, time.Now().UTC())
	})
}

func attachMCPSyncJournalDeps(run *mcpSyncJournalRun, executor *mcpSyncExecutor, deps syncexecpkg.Deps) syncexecpkg.Deps {
	if run == nil {
		return deps
	}
	deps.BeforeItem = run.beforeItem
	deps.AfterItem = func(ctx context.Context, index int, item syncplanpkg.Item, outcome syncexecpkg.Outcome) error {
		return run.afterItem(ctx, index, item, outcome, executor)
	}
	wrap := func(original func(context.Context, syncplanpkg.Item) error, stage syncjournalpkg.MutationStage) func(context.Context, syncplanpkg.Item) error {
		if original == nil {
			return nil
		}
		return func(ctx context.Context, item syncplanpkg.Item) error {
			before, after, err := syncjournalpkg.MutationPhases(item, stage)
			if err != nil {
				return err
			}
			if err := run.setItemPhase(item, before); err != nil {
				return err
			}
			if err := original(ctx, item); err != nil {
				return err
			}
			return run.setItemPhase(item, after)
		}
	}
	deps.CreateRemoteDirectory = wrap(deps.CreateRemoteDirectory, syncjournalpkg.MutationStageWrite)
	deps.CreateLocalDirectory = wrap(deps.CreateLocalDirectory, syncjournalpkg.MutationStageWrite)
	deps.UploadFile = wrap(deps.UploadFile, syncjournalpkg.MutationStageWrite)
	deps.DownloadFile = wrap(deps.DownloadFile, syncjournalpkg.MutationStageWrite)
	deps.RemoveRemote = wrap(deps.RemoveRemote, syncjournalpkg.MutationStageRemove)
	deps.RemoveLocal = wrap(deps.RemoveLocal, syncjournalpkg.MutationStageRemove)
	deps.DeleteRemote = wrap(deps.DeleteRemote, syncjournalpkg.MutationStageDelete)
	deps.DeleteLocal = wrap(deps.DeleteLocal, syncjournalpkg.MutationStageDelete)
	return deps
}

func (run *mcpSyncJournalRun) finalize(summary *syncexecpkg.Summary, executionErr error, recoveryRequired bool) error {
	if run == nil || run.handle == nil {
		return nil
	}
	finishedAt := time.Now().UTC()
	duration := finishedAt.Sub(run.startedAt).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	err := run.handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.RunStats.LastFinishedAt = &finishedAt
		journal.RunStats.LastDurationMillis = duration
		journal.RunStats.TotalDurationMillis += duration
		if recoveryRequired {
			if err := syncjournalpkg.RequireRecovery(journal, "destructive sync execution requires recovery review", finishedAt); err != nil {
				return err
			}
		} else if executionErr != nil || (summary != nil && (summary.Failed > 0 || summary.Blocked > 0)) {
			journal.State = syncjournalpkg.StatusFailed
			if executionErr != nil {
				journal.LastError = executionErr.Error()
			}
		} else {
			journal.State = syncjournalpkg.StatusCompleted
			journal.LastError = ""
			completed := finishedAt
			journal.CompletedAt = &completed
		}
		return nil
	})
	if summary != nil {
		snapshot := run.handle.Snapshot()
		summary.JournalEnabled = true
		summary.JournalResumed = run.resumed
		summary.JournalCompletedBefore = run.completedBefore
		summary.JournalVersion = snapshot.Version
		summary.JournalState = snapshot.State
		summary.JournalStatus = snapshot.Status
	}
	return err
}
