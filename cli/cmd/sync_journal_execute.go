package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

func (handle *syncJournalHandle) setItemPhase(item syncPlanItem, phase string) error {
	return handle.mutate(func(journal *syncExecutionJournal) error {
		index := -1
		key := syncPathKey(item.RelativePath)
		for candidate, planned := range journal.Plan.Items {
			if syncPathKey(planned.RelativePath) == key {
				index = candidate
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("sync journal has no item for %q", item.RelativePath)
		}
		return syncjournalpkg.SetItemPhase(journal, index, phase, time.Now().UTC())
	})
}

func (handle *syncJournalHandle) beforeItem(_ context.Context, index int, item syncPlanItem) error {
	return handle.mutate(func(journal *syncExecutionJournal) error {
		if index < 0 || index >= len(journal.Plan.Items) || syncPathKey(journal.Plan.Items[index].RelativePath) != syncPathKey(item.RelativePath) {
			return fmt.Errorf("sync journal item index %d does not match %q", index, item.RelativePath)
		}
		return syncjournalpkg.BeginItem(journal, index, item, time.Now().UTC())
	})
}

func (handle *syncJournalHandle) afterItem(ctx context.Context, index int, item syncPlanItem, outcome syncExecutionItemOutcome, planClient syncPlanClient) error {
	if index < 0 {
		return fmt.Errorf("sync journal item index %d is invalid", index)
	}
	snapshot := handle.snapshot()
	if index >= len(snapshot.Items) {
		return fmt.Errorf("sync journal item index %d is out of range", index)
	}
	stored := snapshot.Items[index]
	original := snapshot.Plan.Items[index]
	if (stored.State == "succeeded" || stored.State == "skipped") && item.Action == "skip" {
		return nil
	}
	if outcome.Err != nil {
		return handle.mutate(func(journal *syncExecutionJournal) error {
			return syncjournalpkg.FailItem(journal, index, outcome.Err.Error(), time.Now().UTC())
		})
	}
	if original.Action == "skip" {
		return handle.mutate(func(journal *syncExecutionJournal) error {
			return syncjournalpkg.SucceedItem(journal, index, item, nil, time.Now().UTC())
		})
	}
	post, err := captureSyncJournalPostcondition(ctx, planClient, original)
	if err != nil {
		message := "capture postcondition: " + err.Error()
		_ = handle.mutate(func(journal *syncExecutionJournal) error {
			return syncjournalpkg.FailItem(journal, index, message, time.Now().UTC())
		})
		return fmt.Errorf("persist sync journal postcondition for %q: %w", original.RelativePath, err)
	}
	return handle.mutate(func(journal *syncExecutionJournal) error {
		return syncjournalpkg.SucceedItem(journal, index, item, post, time.Now().UTC())
	})
}

func attachSyncJournalExecutionDeps(handle *syncJournalHandle, planClient syncPlanClient, deps syncExecutionDeps) syncExecutionDeps {
	if handle == nil {
		return deps
	}
	deps.beforeItem = handle.beforeItem
	deps.afterItem = func(ctx context.Context, index int, item syncPlanItem, outcome syncExecutionItemOutcome) error {
		return handle.afterItem(ctx, index, item, outcome, planClient)
	}
	wrap := func(original func(context.Context, syncPlanItem) error, stage syncjournalpkg.MutationStage) func(context.Context, syncPlanItem) error {
		if original == nil {
			return nil
		}
		return func(ctx context.Context, item syncPlanItem) error {
			before, after, err := syncjournalpkg.MutationPhases(item, stage)
			if err != nil {
				return err
			}
			if err := handle.setItemPhase(item, before); err != nil {
				return err
			}
			if err := original(ctx, item); err != nil {
				return err
			}
			if err := handle.setItemPhase(item, after); err != nil {
				return err
			}
			return nil
		}
	}
	deps.createRemoteDirectory = wrap(deps.createRemoteDirectory, syncjournalpkg.MutationStageWrite)
	deps.createLocalDirectory = wrap(deps.createLocalDirectory, syncjournalpkg.MutationStageWrite)
	deps.uploadFile = wrap(deps.uploadFile, syncjournalpkg.MutationStageWrite)
	deps.downloadFile = wrap(deps.downloadFile, syncjournalpkg.MutationStageWrite)
	deps.removeRemote = wrap(deps.removeRemote, syncjournalpkg.MutationStageRemove)
	deps.removeLocal = wrap(deps.removeLocal, syncjournalpkg.MutationStageRemove)
	deps.deleteRemote = wrap(deps.deleteRemote, syncjournalpkg.MutationStageDelete)
	deps.deleteLocal = wrap(deps.deleteLocal, syncjournalpkg.MutationStageDelete)
	return deps
}

func captureSyncJournalPostcondition(ctx context.Context, planClient syncPlanClient, item syncPlanItem) (*syncJournalPostcondition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch item.Action {
	case "upload", "replace-remote":
		post, exists, err := captureSyncRemotePostcondition(planClient, item.RemotePath)
		if err != nil {
			return nil, err
		}
		if !exists || post.Kind != item.Kind {
			return nil, fmt.Errorf("remote target %q does not satisfy completed %s action", item.RemotePath, item.Action)
		}
		return post, nil
	case "download", "replace-local":
		post, exists, err := captureSyncLocalPostcondition(item.LocalPath)
		if err != nil {
			return nil, err
		}
		if !exists || post.Kind != item.Kind {
			return nil, fmt.Errorf("local target %q does not satisfy completed %s action", item.LocalPath, item.Action)
		}
		return post, nil
	case "delete-remote":
		_, exists, err := captureSyncRemotePostcondition(planClient, item.RemotePath)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("remote delete target %q still exists after execution", item.RemotePath)
		}
		return &syncJournalPostcondition{Side: "remote", Exists: false}, nil
	case "delete-local":
		_, exists, err := captureSyncLocalPostcondition(item.LocalPath)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("local delete target %q still exists after execution", item.LocalPath)
		}
		return &syncJournalPostcondition{Side: "local", Exists: false}, nil
	case "skip":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported sync journal action %q", item.Action)
	}
}

func captureSyncLocalPostcondition(localPath string) (*syncJournalPostcondition, bool, error) {
	info, err := os.Lstat(localPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("local sync target %q is a symlink", localPath)
	}
	post := &syncJournalPostcondition{Side: "local", Exists: true}
	if info.IsDir() {
		post.Kind = "directory"
		return post, true, nil
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("local sync target %q is not a regular file", localPath)
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, false, err
	}
	digest, digestErr := uploadpkg.PrepareFileDigest(file, info.Size())
	_ = file.Close()
	if digestErr != nil {
		return nil, false, digestErr
	}
	post.Kind = "file"
	post.Size = info.Size()
	post.SHA1 = strings.ToUpper(strings.TrimSpace(digest.SHA1))
	post.ModTimeUnixNano = info.ModTime().UnixNano()
	return post, true, nil
}

func captureSyncRemotePostcondition(planClient syncPlanClient, remotePath string) (*syncJournalPostcondition, bool, error) {
	parentPath := pathpkg.Dir(remotePath)
	parentID, err := resolver.ResolveDir(planClient, parentPath)
	if err != nil {
		return nil, false, fmt.Errorf("resolve remote parent %q while capturing sync journal postcondition: %w", parentPath, err)
	}
	entries, err := listRemoteDirectoryReadOnly(planClient, parentID)
	if err != nil {
		return nil, false, err
	}
	if entries == nil {
		return nil, false, fmt.Errorf("inspect remote parent %q: empty listing response", parentPath)
	}
	name := pathpkg.Base(remotePath)
	matchIndex := -1
	for index, entry := range *entries {
		if entry.Name != name {
			continue
		}
		if matchIndex >= 0 {
			return nil, false, fmt.Errorf("remote sync target %q is ambiguous after execution: multiple entries share the same name", remotePath)
		}
		matchIndex = index
	}
	if matchIndex < 0 {
		return nil, false, nil
	}
	entry := (*entries)[matchIndex]
	post := &syncJournalPostcondition{Side: "remote", Exists: true, RemoteID: entry.FileID}
	if entry.IsDirectory {
		post.Kind = "directory"
		return post, true, nil
	}
	post.Kind = "file"
	file, getErr := planClient.GetFile(entry.FileID)
	if getErr != nil {
		return nil, false, getErr
	}
	if file == nil || file.IsDirectory {
		return nil, false, fmt.Errorf("remote sync target %q changed type while capturing journal postcondition", remotePath)
	}
	post.Size = file.Size
	post.SHA1 = strings.ToUpper(strings.TrimSpace(file.Sha1))
	if !file.UpdateTime.IsZero() {
		post.ModTimeUnixNano = file.UpdateTime.UnixNano()
	}
	return post, true, nil
}

func syncJournalPostconditionEqual(expected, actual *syncJournalPostcondition) bool {
	return syncjournalpkg.PostconditionEqual(expected, actual)
}

func syncJournalPendingPostcondition(ctx context.Context, planClient syncPlanClient, item syncPlanItem) (*syncJournalPostcondition, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	switch item.Action {
	case "upload":
		if err := syncValidateLocalSnapshot(item, item.Kind); err != nil {
			return nil, false, err
		}
		post, exists, err := captureSyncRemotePostcondition(planClient, item.RemotePath)
		if err != nil || !exists || post.Kind != item.Kind {
			return nil, false, err
		}
		if item.Kind == "directory" {
			return post, true, nil
		}
		file, err := os.Open(item.LocalPath)
		if err != nil {
			return nil, false, err
		}
		digest, digestErr := uploadpkg.PrepareFileDigest(file, item.LocalSize)
		_ = file.Close()
		if digestErr != nil {
			return nil, false, digestErr
		}
		if post.Size != item.LocalSize || post.SHA1 == "" || !strings.EqualFold(post.SHA1, digest.SHA1) {
			return nil, false, nil
		}
		return post, true, nil
	case "download":
		if err := syncValidateRemoteSnapshot(planClient, item, item.Kind); err != nil {
			return nil, false, err
		}
		post, exists, err := captureSyncLocalPostcondition(item.LocalPath)
		if err != nil || !exists || post.Kind != item.Kind {
			return nil, false, err
		}
		if item.Kind == "directory" {
			return post, true, nil
		}
		expectedSHA1 := strings.ToUpper(strings.TrimSpace(item.RemoteSHA1))
		if expectedSHA1 == "" {
			remote, getErr := planClient.GetFile(item.RemoteID)
			if getErr != nil {
				return nil, false, getErr
			}
			if remote != nil {
				expectedSHA1 = strings.ToUpper(strings.TrimSpace(remote.Sha1))
			}
		}
		if post.Size != item.RemoteSize || expectedSHA1 == "" || !strings.EqualFold(post.SHA1, expectedSHA1) {
			return nil, false, nil
		}
		return post, true, nil
	default:
		return nil, false, nil
	}
}

const (
	syncJournalDestructiveCompleted  = string(syncjournalpkg.DestructiveCompleted)
	syncJournalDestructiveRetryFull  = string(syncjournalpkg.DestructiveRetryFull)
	syncJournalDestructiveWinnerOnly = string(syncjournalpkg.DestructiveWinnerOnly)
	syncJournalDestructiveAmbiguous  = string(syncjournalpkg.DestructiveAmbiguous)
)

func captureSyncJournalActionTarget(planClient syncPlanClient, item syncPlanItem) (*syncJournalPostcondition, bool, error) {
	switch item.Action {
	case "delete-remote", "replace-remote":
		return captureSyncRemotePostcondition(planClient, item.RemotePath)
	case "delete-local", "replace-local":
		return captureSyncLocalPostcondition(item.LocalPath)
	default:
		return nil, false, fmt.Errorf("unsupported destructive sync journal action %q", item.Action)
	}
}

func syncJournalReplacementWinnerMatches(ctx context.Context, planClient syncPlanClient, item syncPlanItem, post *syncJournalPostcondition) (bool, error) {
	if post == nil || !post.Exists || post.Kind != item.Kind {
		return false, nil
	}
	if item.Kind == "directory" {
		return true, nil
	}
	switch item.Action {
	case "replace-remote":
		if err := syncValidateLocalSnapshot(item, item.Kind); err != nil {
			return false, err
		}
		expected := strings.ToUpper(strings.TrimSpace(item.LocalSHA1))
		if expected == "" {
			file, err := os.Open(item.LocalPath)
			if err != nil {
				return false, err
			}
			digest, digestErr := uploadpkg.PrepareFileDigest(file, item.LocalSize)
			_ = file.Close()
			if digestErr != nil {
				return false, digestErr
			}
			expected = strings.ToUpper(strings.TrimSpace(digest.SHA1))
		}
		return post.Size == item.LocalSize && post.SHA1 != "" && strings.EqualFold(post.SHA1, expected), nil
	case "replace-local":
		if err := syncValidateRemoteSnapshot(planClient, item, item.Kind); err != nil {
			return false, err
		}
		expected := strings.ToUpper(strings.TrimSpace(item.RemoteSHA1))
		if expected == "" {
			remote, err := planClient.GetFile(item.RemoteID)
			if err != nil {
				return false, err
			}
			if remote != nil {
				expected = strings.ToUpper(strings.TrimSpace(remote.Sha1))
			}
		}
		if expected == "" {
			return false, nil
		}
		return post.Size == item.RemoteSize && post.SHA1 != "" && strings.EqualFold(post.SHA1, expected), nil
	default:
		return false, fmt.Errorf("unsupported replacement action %q", item.Action)
	}
}

func syncJournalOriginalTargetMatches(planClient syncPlanClient, plan syncPlan, item syncPlanItem, post *syncJournalPostcondition) (bool, error) {
	if post == nil || !post.Exists {
		return false, nil
	}
	expectedKind := item.Kind
	if item.Action == "replace-remote" || item.Action == "replace-local" {
		expectedKind = item.ReplacesKind
	}
	if post.Kind != expectedKind {
		return false, nil
	}
	switch item.Action {
	case "delete-remote", "replace-remote":
		if item.RemoteID == "" || post.RemoteID != item.RemoteID {
			return false, nil
		}
		if expectedKind == "file" {
			if post.Size != item.RemoteSize {
				return false, nil
			}
			if item.RemoteSHA1 != "" && (post.SHA1 == "" || !strings.EqualFold(post.SHA1, item.RemoteSHA1)) {
				return false, nil
			}
			return true, nil
		}
		if err := syncValidateRemoteReplacementSubtree(planClient, plan, item); err != nil {
			return false, err
		}
		return true, nil
	case "delete-local", "replace-local":
		if expectedKind == "file" {
			if post.Size != item.LocalSize {
				return false, nil
			}
			if item.LocalModTimeUnixNano != 0 && post.ModTimeUnixNano != item.LocalModTimeUnixNano {
				return false, nil
			}
			return true, nil
		}
		if err := syncValidateLocalReplacementSubtree(plan, item); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported destructive action %q", item.Action)
	}
}

func syncJournalValidateReplacementWinnerSource(planClient syncPlanClient, item syncPlanItem) error {
	switch item.Action {
	case "replace-remote":
		return syncValidateLocalSnapshot(item, item.Kind)
	case "replace-local":
		return syncValidateRemoteSnapshot(planClient, item, item.Kind)
	default:
		return fmt.Errorf("unsupported replacement action %q", item.Action)
	}
}

func reconcileSyncJournalDestructiveItem(ctx context.Context, planClient syncPlanClient, plan syncPlan, item syncPlanItem) (*syncJournalPostcondition, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	post, exists, err := captureSyncJournalActionTarget(planClient, item)
	if err != nil {
		return nil, "", err
	}
	winnerMatches := false
	originalMatches := false
	switch item.Action {
	case "delete-remote", "delete-local":
		if exists {
			originalMatches, err = syncJournalOriginalTargetMatches(planClient, plan, item, post)
			if err != nil {
				return nil, "", err
			}
		}
	case "replace-remote", "replace-local":
		if exists {
			winnerMatches, err = syncJournalReplacementWinnerMatches(ctx, planClient, item, post)
			if err != nil {
				return nil, "", err
			}
			if !winnerMatches {
				originalMatches, err = syncJournalOriginalTargetMatches(planClient, plan, item, post)
				if err != nil {
					return nil, "", err
				}
			}
		} else if err := syncJournalValidateReplacementWinnerSource(planClient, item); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", fmt.Errorf("unsupported destructive action %q", item.Action)
	}
	decision, err := syncjournalpkg.ClassifyDestructiveEvidence(item.Action, exists, winnerMatches, originalMatches)
	if err != nil {
		return nil, "", err
	}
	if decision == syncjournalpkg.DestructiveCompleted {
		if item.Action == "delete-remote" || item.Action == "delete-local" {
			side := "remote"
			if item.Action == "delete-local" {
				side = "local"
			}
			return &syncJournalPostcondition{Side: side, Exists: false}, string(decision), nil
		}
		return post, string(decision), nil
	}
	return nil, string(decision), nil
}

func (handle *syncJournalHandle) reconcileForResume(ctx context.Context, planClient syncPlanClient) error {
	return handle.reconcileForResumeWithReview(ctx, planClient, false, false)
}

func (handle *syncJournalHandle) reconcileForResumeAfterReview(ctx context.Context, planClient syncPlanClient) error {
	return handle.reconcileForResumeWithReview(ctx, planClient, true, true)
}

func (handle *syncJournalHandle) reconcileForResumeWithReview(ctx context.Context, planClient syncPlanClient, allowRecovery, requirePreflightBeforePersist bool) error {
	if handle == nil {
		return errors.New("sync journal handle is nil")
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	next := cloneSyncExecutionJournal(handle.journal)
	journal := &next
	if journal.State == "completed" {
		return fmt.Errorf("%w: %s", errSyncJournalCompleted, journal.PlanID)
	}
	initialRecovery := syncJournalRecoveryRequired(*journal)
	if initialRecovery && !allowRecovery {
		return fmt.Errorf("%w: inspect or verify journal %s before retrying", errSyncJournalRecoveryRequired, journal.PlanID)
	}
	persistRecovery := func(message string) error {
		journal.State = "recovery-required"
		journal.LastError = message
		journal.UpdatedAt = time.Now().UTC()
		recoveryErr := fmt.Errorf("%w: %s", errSyncJournalRecoveryRequired, message)
		if err := handle.store.write(handle.location, *journal); err != nil {
			return errors.Join(recoveryErr, fmt.Errorf("persist recovery-required sync journal: %w", err))
		}
		handle.journal = next
		return recoveryErr
	}
	changed := false
	for index := range journal.Items {
		stored := &journal.Items[index]
		item := journal.Plan.Items[index]
		if stored.State == "succeeded" || stored.State == "skipped" {
			continue
		}
		if syncPlanItemDestructive(item) {
			post, decision, err := reconcileSyncJournalDestructiveItem(ctx, planClient, journal.Plan, item)
			if err != nil {
				return fmt.Errorf("reconcile destructive sync journal item %q: %w", item.RelativePath, err)
			}
			if decision == syncJournalDestructiveAmbiguous {
				return persistRecovery(fmt.Sprintf("destructive item %q no longer matches either its planned loser or its verifiable winner", item.RelativePath))
			}
			if err := syncjournalpkg.ApplyDestructiveDecision(journal, index, syncjournalpkg.DestructiveDecision(decision), post, time.Now().UTC()); err != nil {
				return fmt.Errorf("apply destructive sync journal decision for %q: %w", item.RelativePath, err)
			}
			changed = true
			continue
		}

		post, completed, err := syncJournalPendingPostcondition(ctx, planClient, item)
		if err != nil {
			return fmt.Errorf("reconcile sync journal item %q: %w", item.RelativePath, err)
		}
		if completed {
			stored.State = "succeeded"
			stored.Phase = syncjournalpkg.PhaseDone
			stored.Post = post
			stored.LastError = ""
			stored.UpdatedAt = time.Now().UTC()
			changed = true
			continue
		}
		if stored.Phase == syncjournalpkg.PhaseMutationDone {
			return fmt.Errorf("reconcile sync journal item %q: completed mutation postcondition is not yet verifiable; retry resume after the target state becomes observable", item.RelativePath)
		}
		phase := syncjournalpkg.PhasePending
		if stored.Phase == syncjournalpkg.PhaseMutationStarted {
			phase = syncjournalpkg.PhaseMutationStarted
		}
		if stored.State != "pending" || stored.Phase != phase || stored.LastError != "" {
			stored.State = "pending"
			stored.Phase = phase
			stored.Post = nil
			stored.LastError = ""
			stored.UpdatedAt = time.Now().UTC()
			changed = true
		}
	}
	if initialRecovery && allowRecovery {
		changed = true
	}
	if changed {
		journal.State = "active"
		journal.LastError = ""
		journal.CompletedAt = nil
		journal.UpdatedAt = time.Now().UTC()
		if requirePreflightBeforePersist {
			if err := preflightSyncJournalResume(ctx, planClient, *journal); err != nil {
				return fmt.Errorf("reviewed sync journal still fails resume preflight: %w", err)
			}
		}
		if err := handle.store.write(handle.location, *journal); err != nil {
			return err
		}
		handle.journal = next
	}
	return nil
}

func buildSyncJournalExpectedPlan(journal syncExecutionJournal) (syncPlan, error) {
	return syncjournalpkg.ExpectedPlan(journal)
}

func buildSyncJournalResidualPlan(journal syncExecutionJournal) syncPlan {
	return syncjournalpkg.ResidualPlan(journal)
}

func validateSyncJournalPostconditions(ctx context.Context, planClient syncPlanClient, journal syncExecutionJournal) error {
	for index, stored := range journal.Items {
		if stored.State != "succeeded" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		item := journal.Plan.Items[index]
		if stored.Post == nil {
			return fmt.Errorf("completed sync journal item %q has no postcondition", item.RelativePath)
		}
		var (
			actual *syncJournalPostcondition
			exists bool
			err    error
		)
		if stored.Post.Side == "remote" {
			actual, exists, err = captureSyncRemotePostcondition(planClient, item.RemotePath)
		} else {
			actual, exists, err = captureSyncLocalPostcondition(item.LocalPath)
		}
		if err != nil {
			return fmt.Errorf("validate completed journal item %q: %w", item.RelativePath, err)
		}
		if !stored.Post.Exists && !exists {
			continue
		}
		if !exists || !syncJournalPostconditionEqual(stored.Post, actual) {
			return fmt.Errorf("completed sync journal item %q no longer satisfies its recorded postcondition", item.RelativePath)
		}
	}
	return nil
}

func preflightSyncJournalResume(ctx context.Context, planClient syncPlanClient, journal syncExecutionJournal) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSyncPreflightRoots(planClient, journal.Plan); err != nil {
		return err
	}
	expected, err := buildSyncJournalExpectedPlan(journal)
	if err != nil {
		return err
	}
	for _, item := range expected.Items {
		if err := ensureSyncLocalPathWithinRoot(expected.LocalRoot, item.LocalPath); err != nil {
			return err
		}
		if err := ensureSyncRemotePathWithinRoot(expected.RemoteRoot, item.RemotePath); err != nil {
			return err
		}
	}
	if err := validateSyncJournalPostconditions(ctx, planClient, journal); err != nil {
		return err
	}
	localTree, err := scanLocalUploadTree(expected.LocalRoot)
	if err != nil {
		return fmt.Errorf("rescan local sync tree for resume: %w", err)
	}
	currentLocal, err := collectLocalSyncEntries(localTree, expected.RemoteRoot)
	if err != nil {
		return err
	}
	removeSyncJournalDownloadArtifacts(currentLocal, journal)
	if err := compareSyncPreflightLocalTree(expected, currentLocal); err != nil {
		return err
	}
	currentRemote, err := collectRemoteSyncEntries(planClient, expected.RemoteRootID, expected.RemoteRoot, expected.LocalRoot)
	if err != nil {
		return err
	}
	if err := compareSyncPreflightRemoteTree(planClient, expected, currentRemote); err != nil {
		return err
	}
	return ctx.Err()
}

func removeSyncJournalDownloadArtifacts(current map[string]syncTreeEntry, journal syncExecutionJournal) {
	for index, stored := range journal.Items {
		item := journal.Plan.Items[index]
		if item.Kind == "directory" {
			continue
		}
		interruptedDownload := item.Action == "download" && stored.Phase == syncjournalpkg.PhaseMutationStarted
		interruptedReplacementDownload := item.Action == "replace-local" && (stored.Phase == syncjournalpkg.PhaseWinnerStarted || stored.Phase == syncjournalpkg.PhaseLoserRemoved || stored.Phase == syncjournalpkg.PhaseMutationStarted)
		if !interruptedDownload && !interruptedReplacementDownload {
			continue
		}
		relative := filepath.ToSlash(item.RelativePath)
		dir := pathpkg.Dir(relative)
		if dir == "." {
			dir = ""
		}
		base := pathpkg.Base(relative)
		for _, artifact := range []string{"." + base + ".115driver.part", "." + base + ".115driver.resume.json"} {
			candidate := artifact
			if dir != "" {
				candidate = dir + "/" + artifact
			}
			delete(current, syncPathKey(candidate))
		}
	}
}

func (handle *syncJournalHandle) beginRun(resumed bool) error {
	return handle.mutate(func(journal *syncExecutionJournal) error {
		now := time.Now().UTC()
		stats := &journal.RunStats
		if stats.LastStartedAt != nil && (stats.LastFinishedAt == nil || stats.LastFinishedAt.Before(*stats.LastStartedAt)) {
			stats.InterruptedRuns++
		}
		stats.Runs++
		if resumed {
			stats.ResumeRuns++
		}
		startedAt := now
		stats.LastStartedAt = &startedAt
		stats.LastFinishedAt = nil
		stats.LastDurationMillis = 0
		journal.State = "active"
		journal.CompletedAt = nil
		journal.LastError = ""
		return nil
	})
}

func (handle *syncJournalHandle) finishRun(summary syncExecutionSummary, runErr error) error {
	return handle.mutate(func(journal *syncExecutionJournal) error {
		now := time.Now().UTC()
		stats := &journal.RunStats
		if stats.LastStartedAt != nil && stats.LastFinishedAt == nil {
			duration := now.Sub(*stats.LastStartedAt)
			if duration < 0 {
				duration = 0
			}
			finishedAt := now
			stats.LastFinishedAt = &finishedAt
			stats.LastDurationMillis = duration.Milliseconds()
			stats.TotalDurationMillis += stats.LastDurationMillis
		}
		byPath := make(map[string]syncExecutionItemResult, len(summary.Items))
		for _, result := range summary.Items {
			byPath[syncPathKey(result.RelativePath)] = result
		}
		for index := range journal.Items {
			stored := &journal.Items[index]
			result, ok := byPath[syncPathKey(stored.RelativePath)]
			if !ok || stored.State == "succeeded" || stored.State == "skipped" {
				continue
			}
			switch result.Status {
			case "blocked":
				stored.State = "blocked"
				stored.LastError = result.Error
				stored.UpdatedAt = time.Now().UTC()
			case "failed":
				stored.State = "failed"
				stored.LastError = result.Error
				stored.UpdatedAt = time.Now().UTC()
			}
		}
		if runErr == nil {
			journal.State = "completed"
			journal.CompletedAt = &now
			journal.LastError = ""
			return nil
		}
		journal.CompletedAt = nil
		journal.LastError = runErr.Error()
		if journal.State != "recovery-required" {
			journal.State = "failed"
		}
		return nil
	})
}
