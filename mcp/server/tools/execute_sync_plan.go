package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"strings"
	"sync/atomic"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
	syncguardpkg "github.com/SheltonZhu/115driver/internal/syncguard"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPSyncExecutionJobs = 16

var (
	errMCPSyncPlanChanged          = errors.New("sync plan changed after review")
	errMCPSyncDeleteBudgetExceeded = errors.New("sync mirror-delete execution budget exceeded")
	errMCPSyncRecoveryRequired     = errors.New("sync destructive state requires recovery review")
	errMCPSyncJournalStateChanged  = errors.New("sync journal state changed after review")
)

// ExecuteSyncPlanArgs repeats the reviewed plan_sync inputs. Current state is
// replanned before execution and again at the shared syncexec preflight barrier
// immediately before the first write. When a sync journal store is configured,
// execution also persists the shared current-schema journal used by CLI sync.
type ExecuteSyncPlanArgs struct {
	LocalPath        string `json:"local_path"`
	RemotePath       string `json:"remote_path"`
	Direction        string `json:"direction,omitempty"`
	ConflictPolicy   string `json:"conflict_policy,omitempty"`
	DeleteExtraneous bool   `json:"delete,omitempty"`
	MaxNodes         int    `json:"max_nodes,omitempty"`
	MaxChecksumBytes int64  `json:"max_checksum_bytes,omitempty"`
	MaxDeleteRoots   int    `json:"max_delete_roots,omitempty" jsonschema:"execution-only destructive removal root budget covering mirror deletes and replacement old targets; 0 is unlimited"`
	MaxDeleteItems   int    `json:"max_delete_items,omitempty" jsonschema:"execution-only destructive removal affected-item budget including covered directory descendants; 0 is unlimited"`
	MaxDeleteBytes   int64  `json:"max_delete_bytes,omitempty" jsonschema:"execution-only destructive removal file-byte budget including covered directory descendants; 0 is unlimited"`
	ExpectPlanID     string `json:"expect_plan_id" jsonschema:"required reviewed MCPPlan v1 plan_id returned by plan_sync"`
	Jobs             int    `json:"jobs,omitempty" jsonschema:"maximum concurrently scheduled sync actions; default 1, maximum 16; file transfers are additionally serialized in this MCP executor"`
	ContinueOnError  bool   `json:"continue_on_error,omitempty" jsonschema:"continue independent dependency branches after an item failure"`
	MaxErrors        int    `json:"max_errors,omitempty" jsonschema:"optional positive failure cap; requires continue_on_error"`
}

func (args ExecuteSyncPlanArgs) planSyncArgs() PlanSyncArgs {
	return PlanSyncArgs{
		LocalPath:        args.LocalPath,
		RemotePath:       args.RemotePath,
		Direction:        args.Direction,
		ConflictPolicy:   args.ConflictPolicy,
		DeleteExtraneous: args.DeleteExtraneous,
		MaxNodes:         args.MaxNodes,
		MaxChecksumBytes: args.MaxChecksumBytes,
	}
}

type MCPSyncExecutionSummary struct {
	PlannedItems             int    `json:"planned_items"`
	Processed                int    `json:"processed"`
	Succeeded                int    `json:"succeeded"`
	Skipped                  int    `json:"skipped"`
	Failed                   int    `json:"failed"`
	Blocked                  int    `json:"blocked"`
	UploadedFiles            int    `json:"uploaded_files"`
	CreatedRemoteDirectories int    `json:"created_remote_directories"`
	DownloadedFiles          int    `json:"downloaded_files"`
	CreatedLocalDirectories  int    `json:"created_local_directories"`
	ReplacedRemote           int    `json:"replaced_remote"`
	ReplacedLocal            int    `json:"replaced_local"`
	DeletedRemote            int    `json:"deleted_remote"`
	DeletedLocal             int    `json:"deleted_local"`
	DestructiveActions       int    `json:"destructive_actions"`
	Jobs                     int    `json:"jobs"`
	FileTransferSlots        int    `json:"file_transfer_slots"`
	PreflightChecked         int    `json:"preflight_checked"`
	PreflightPassed          bool   `json:"preflight_passed"`
	JournalPersisted         bool   `json:"journal_persisted"`
	JournalResumed           bool   `json:"journal_resumed"`
	JournalCompletedBefore   int    `json:"journal_completed_before"`
	JournalVersion           int    `json:"journal_version,omitempty"`
	JournalState             string `json:"journal_state,omitempty"`
	JournalStatus            string `json:"journal_status,omitempty"`
}

type MCPSyncExecutionOutput struct {
	PlanID           string                   `json:"plan_id,omitempty" jsonschema:"reviewed MCP plan ID, returned only when the initial live replan matched"`
	Summary          MCPSyncExecutionSummary  `json:"summary"`
	Items            []syncexecpkg.ItemResult `json:"items,omitempty" jsonschema:"safe relative-path execution results in plan order"`
	RecoveryRequired bool                     `json:"recovery_required,omitempty" jsonschema:"true when a destructive mutation may have partially or ambiguously completed; callers must re-plan before retrying"`
	ErrorCode        string                   `json:"error_code,omitempty"`
	Error            string                   `json:"error,omitempty" jsonschema:"sanitized execution error"`
}

func normalizeMCPSyncExecutionJobs(value int) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("jobs must be >= 0")
	}
	if value == 0 {
		value = 1
	}
	if value > maxMCPSyncExecutionJobs {
		return 0, fmt.Errorf("jobs must not exceed %d", maxMCPSyncExecutionJobs)
	}
	return value, nil
}

type mcpSyncDeleteBudget struct {
	maxRoots int
	maxItems int
	maxBytes int64
}

func normalizeMCPSyncDeleteBudget(args ExecuteSyncPlanArgs) (mcpSyncDeleteBudget, error) {
	if args.MaxDeleteRoots < 0 {
		return mcpSyncDeleteBudget{}, fmt.Errorf("max_delete_roots must be >= 0")
	}
	if args.MaxDeleteItems < 0 {
		return mcpSyncDeleteBudget{}, fmt.Errorf("max_delete_items must be >= 0")
	}
	if args.MaxDeleteBytes < 0 {
		return mcpSyncDeleteBudget{}, fmt.Errorf("max_delete_bytes must be >= 0")
	}
	budget := mcpSyncDeleteBudget{maxRoots: args.MaxDeleteRoots, maxItems: args.MaxDeleteItems, maxBytes: args.MaxDeleteBytes}
	return budget, nil
}

func (budget mcpSyncDeleteBudget) validate(plan syncplanpkg.Plan) error {
	roots, items, bytes := plan.DeleteTotals()
	if budget.maxRoots > 0 && roots > budget.maxRoots {
		return fmt.Errorf("%w: planned %d root(s) exceed max_delete_roots %d", errMCPSyncDeleteBudgetExceeded, roots, budget.maxRoots)
	}
	if budget.maxItems > 0 && items > budget.maxItems {
		return fmt.Errorf("%w: planned %d affected item(s) exceed max_delete_items %d", errMCPSyncDeleteBudgetExceeded, items, budget.maxItems)
	}
	if budget.maxBytes > 0 && bytes > budget.maxBytes {
		return fmt.Errorf("%w: planned %d byte(s) exceed max_delete_bytes %d", errMCPSyncDeleteBudgetExceeded, bytes, budget.maxBytes)
	}
	return nil
}

func validateMCPSyncSnapshotKind(kind, role, relativePath string) error {
	if kind != "file" && kind != "directory" {
		return fmt.Errorf("planned %s %q has unsupported kind %q", role, relativePath, kind)
	}
	return nil
}

func validateMCPSyncLocalIdentity(item syncplanpkg.Item, kind, role string, requireContent bool) error {
	if err := validateMCPSyncSnapshotKind(kind, role, item.RelativePath); err != nil {
		return err
	}
	if !item.LocalPresent || strings.TrimSpace(item.LocalPath) == "" {
		return fmt.Errorf("planned %s %q has incomplete local identity", role, item.RelativePath)
	}
	if requireContent && kind == "file" && strings.TrimSpace(item.LocalSHA1) == "" {
		return fmt.Errorf("planned %s %q has no local content snapshot", role, item.RelativePath)
	}
	return nil
}

func validateMCPSyncRemoteIdentity(item syncplanpkg.Item, kind, role string, requireContent bool) error {
	if err := validateMCPSyncSnapshotKind(kind, role, item.RelativePath); err != nil {
		return err
	}
	if !item.RemotePresent || strings.TrimSpace(item.RemoteID) == "" || strings.TrimSpace(item.RemotePath) == "" {
		return fmt.Errorf("planned %s %q has incomplete remote identity", role, item.RelativePath)
	}
	if requireContent && kind == "file" && strings.TrimSpace(item.RemoteSHA1) == "" {
		return fmt.Errorf("planned %s %q has no remote content snapshot", role, item.RelativePath)
	}
	return nil
}

func validateMCPSyncDestructiveDirectorySnapshot(plan syncplanpkg.Plan, item syncplanpkg.Item, side syncexecpkg.SubtreeSide) error {
	if _, err := syncexecpkg.ExpectedSubtree(plan, item.RelativePath, side); err != nil {
		return fmt.Errorf("planned destructive %s subtree %q is not fully snapshotted: %w", side, item.RelativePath, err)
	}
	return nil
}

func validateMCPSyncExecutablePlan(plan syncplanpkg.Plan) error {
	for _, item := range plan.Items {
		switch item.Action {
		case "upload":
			if err := validateMCPSyncLocalIdentity(item, item.Kind, "upload source", true); err != nil {
				return err
			}
		case "download":
			if err := validateMCPSyncRemoteIdentity(item, item.Kind, "download source", true); err != nil {
				return err
			}
		case "delete-remote":
			if err := validateMCPSyncRemoteIdentity(item, item.Kind, "remote delete target", true); err != nil {
				return err
			}
			if item.Kind == "directory" {
				if err := validateMCPSyncDestructiveDirectorySnapshot(plan, item, syncexecpkg.SubtreeRemote); err != nil {
					return err
				}
			}
		case "replace-remote":
			if err := validateMCPSyncLocalIdentity(item, item.Kind, "remote replacement winner", true); err != nil {
				return err
			}
			if err := validateMCPSyncRemoteIdentity(item, item.ReplacesKind, "remote replacement target", true); err != nil {
				return err
			}
			if item.ReplacesKind == "directory" {
				if err := validateMCPSyncDestructiveDirectorySnapshot(plan, item, syncexecpkg.SubtreeRemote); err != nil {
					return err
				}
			}
		case "delete-local":
			if err := validateMCPSyncLocalIdentity(item, item.Kind, "local delete target", true); err != nil {
				return err
			}
			if item.Kind == "directory" {
				if err := validateMCPSyncDestructiveDirectorySnapshot(plan, item, syncexecpkg.SubtreeLocal); err != nil {
					return err
				}
			}
		case "replace-local":
			if err := validateMCPSyncRemoteIdentity(item, item.Kind, "local replacement winner", true); err != nil {
				return err
			}
			if err := validateMCPSyncLocalIdentity(item, item.ReplacesKind, "local replacement target", true); err != nil {
				return err
			}
			if item.ReplacesKind == "directory" {
				if err := validateMCPSyncDestructiveDirectorySnapshot(plan, item, syncexecpkg.SubtreeLocal); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func redactMCPSyncExecutionError(err error, ft *FileTools, args PlanSyncArgs, plan syncplanpkg.Plan) string {
	if err == nil {
		return ""
	}
	values := make(map[string]struct{})
	if ft != nil {
		addMCPSyncLocalPathRedactionValues(values, ft.localRoot)
	}
	addMCPSyncLocalPathRedactionValues(values, args.LocalPath)
	addMCPSyncLocalPathRedactionValues(values, plan.LocalRoot)
	addMCPSyncRedactionValue(values, plan.RemoteRootID)
	addMCPSyncRedactionValue(values, plan.RemoteRoot)
	addMCPSyncRedactionValue(values, args.RemotePath)
	for _, item := range plan.Items {
		addMCPSyncLocalPathRedactionValues(values, item.LocalPath)
		addMCPSyncRedactionValue(values, item.RemoteID)
		addMCPSyncRedactionValue(values, item.RemotePath)
		addMCPSyncRedactionValue(values, item.LocalSHA1)
		addMCPSyncRedactionValue(values, item.RemoteSHA1)
	}
	return redactMCPSyncValues(err.Error(), values, "[REDACTED_SYNC_IDENTITY]")
}

func mcpSyncExecutionOutput(expectedPlanID string, summary syncexecpkg.Summary, ft *FileTools, args PlanSyncArgs, plan syncplanpkg.Plan, err error) MCPSyncExecutionOutput {
	output := MCPSyncExecutionOutput{
		PlanID: expectedPlanID,
		Summary: MCPSyncExecutionSummary{
			PlannedItems:             summary.PlannedItems,
			Processed:                summary.Processed,
			Succeeded:                summary.Succeeded,
			Skipped:                  summary.Skipped,
			Failed:                   summary.Failed,
			Blocked:                  summary.Blocked,
			UploadedFiles:            summary.UploadedFiles,
			CreatedRemoteDirectories: summary.CreatedRemoteDirs,
			DownloadedFiles:          summary.DownloadedFiles,
			CreatedLocalDirectories:  summary.CreatedLocalDirs,
			ReplacedRemote:           summary.ReplacedRemote,
			ReplacedLocal:            summary.ReplacedLocal,
			DeletedRemote:            summary.DeletedRemote,
			DeletedLocal:             summary.DeletedLocal,
			DestructiveActions:       summary.DestructiveActions,
			Jobs:                     summary.Jobs,
			FileTransferSlots:        summary.FileTransferSlots,
			PreflightChecked:         summary.PreflightChecked,
			PreflightPassed:          summary.PreflightPassed,
			JournalPersisted:         summary.JournalEnabled,
			JournalResumed:           summary.JournalResumed,
			JournalCompletedBefore:   summary.JournalCompletedBefore,
			JournalVersion:           summary.JournalVersion,
			JournalState:             summary.JournalState,
			JournalStatus:            summary.JournalStatus,
		},
		Items: append([]syncexecpkg.ItemResult(nil), summary.Items...),
	}
	for i := range output.Items {
		if strings.TrimSpace(output.Items[i].Error) != "" {
			output.Items[i].Error = redactMCPSyncExecutionError(errors.New(output.Items[i].Error), ft, args, plan)
		}
	}
	if err != nil {
		output.ErrorCode = "execution_failed"
		output.Error = redactMCPSyncExecutionError(err, ft, args, plan)
	}
	return output
}

type mcpSyncExecutor struct {
	ft                     *FileTools
	plan                   syncplanpkg.Plan
	recoveryRequired       atomic.Bool
	recoveryChecksumBudget *mcpSyncRecoveryChecksumBudget
}

func (executor *mcpSyncExecutor) markRecoveryRequired(err error) error {
	if err == nil {
		return nil
	}
	executor.recoveryRequired.Store(true)
	if errors.Is(err, errMCPSyncRecoveryRequired) {
		return err
	}
	return fmt.Errorf("%w: %w", errMCPSyncRecoveryRequired, err)
}

func (executor *mcpSyncExecutor) markMutationFailure(item syncplanpkg.Item, stage syncjournalpkg.MutationStage, err error) error {
	if err != nil && syncjournalpkg.MutationFailureRequiresRecovery(item, stage) {
		return executor.markRecoveryRequired(err)
	}
	return err
}

func (executor *mcpSyncExecutor) resolver() *remoteresolver.PathResolver {
	return remoteresolver.New(executor.ft.client)
}

func (executor *mcpSyncExecutor) ensureRemoteAbsent(remotePath string) error {
	_, _, err := executor.resolver().ResolvePath(remotePath)
	if err == nil {
		return fmt.Errorf("remote target already exists")
	}
	if errors.Is(err, driver.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("inspect remote target: %w", err)
}

func (executor *mcpSyncExecutor) validateRemoteSnapshot(item syncplanpkg.Item, expectedKind string) (*driver.File, error) {
	id, isDir, err := executor.resolver().ResolvePath(item.RemotePath)
	if err != nil {
		return nil, fmt.Errorf("resolve planned remote object: %w", err)
	}
	if strings.TrimSpace(item.RemoteID) == "" || id != item.RemoteID || isDir != (expectedKind == "directory") {
		return nil, fmt.Errorf("planned remote object changed identity or type")
	}
	file, err := executor.ft.client.GetFile(id)
	if err != nil {
		return nil, fmt.Errorf("read planned remote object: %w", err)
	}
	if file == nil || file.IsDirectory != (expectedKind == "directory") {
		return nil, fmt.Errorf("planned remote object changed type")
	}
	if expectedKind == "file" {
		if file.Size != item.RemoteSize {
			return nil, fmt.Errorf("planned remote file changed size")
		}
		if item.RemoteSHA1 != "" && !strings.EqualFold(strings.TrimSpace(file.Sha1), item.RemoteSHA1) {
			return nil, fmt.Errorf("planned remote file changed content")
		}
	}
	if item.RemoteModTimeUnixNano != 0 && !file.UpdateTime.IsZero() && file.UpdateTime.UnixNano() != item.RemoteModTimeUnixNano {
		return nil, fmt.Errorf("planned remote object changed modification time")
	}
	return file, nil
}

func (executor *mcpSyncExecutor) openLocalFileSnapshot(item syncplanpkg.Item) (*os.File, *uploadpkg.PreparedDigest, error) {
	localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, true)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect planned local file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != item.LocalSize {
		return nil, nil, fmt.Errorf("planned local file changed identity, type, or size")
	}
	if item.LocalModTimeUnixNano != 0 && info.ModTime().UnixNano() != item.LocalModTimeUnixNano {
		return nil, nil, fmt.Errorf("planned local file changed modification time")
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open planned local file: %w", err)
	}
	digest, err := uploadpkg.PrepareFileDigest(file, item.LocalSize)
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("hash planned local file: %w", err)
	}
	if item.LocalSHA1 == "" || !strings.EqualFold(digest.SHA1, item.LocalSHA1) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("planned local file changed content")
	}
	if item.LocalModTimeUnixNano != 0 && digest.ModTimeUnixNano != item.LocalModTimeUnixNano {
		_ = file.Close()
		return nil, nil, fmt.Errorf("planned local file changed while hashing")
	}
	return file, digest, nil
}

func (executor *mcpSyncExecutor) validateLocalDirectorySnapshot(item syncplanpkg.Item) error {
	localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, true)
	if err != nil {
		return err
	}
	info, err := os.Lstat(localPath)
	if err != nil {
		return fmt.Errorf("inspect planned local directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("planned local directory changed identity or type")
	}
	if item.LocalModTimeUnixNano != 0 && info.ModTime().UnixNano() != item.LocalModTimeUnixNano {
		return fmt.Errorf("planned local directory changed modification time")
	}
	return nil
}

func (executor *mcpSyncExecutor) validateLocalFileSnapshot(item syncplanpkg.Item) error {
	file, _, err := executor.openLocalFileSnapshot(item)
	if file != nil {
		_ = file.Close()
	}
	return err
}

func (executor *mcpSyncExecutor) validateLocalWinnerSnapshot(item syncplanpkg.Item) error {
	if item.Kind == "directory" {
		return executor.validateLocalDirectorySnapshot(item)
	}
	return executor.validateLocalFileSnapshot(item)
}

func (executor *mcpSyncExecutor) validateRemoteSubtree(ctx context.Context, item syncplanpkg.Item) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if _, err := executor.validateRemoteSnapshot(item, "directory"); err != nil {
		return err
	}
	if err := syncguardpkg.ValidateRemoteSubtree(executor.ft.client, executor.plan, item); err != nil {
		return fmt.Errorf("validate planned remote subtree: %w", err)
	}
	return nil
}

func (executor *mcpSyncExecutor) validateLocalSubtree(item syncplanpkg.Item) error {
	if err := executor.validateLocalDirectorySnapshot(item); err != nil {
		return err
	}
	if err := syncguardpkg.ValidateLocalSubtree(executor.plan, item); err != nil {
		return fmt.Errorf("validate planned local subtree: %w", err)
	}
	return nil
}

func (executor *mcpSyncExecutor) ensureLocalAbsent(item syncplanpkg.Item) (string, error) {
	localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, false)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(localPath); err == nil {
		return "", fmt.Errorf("local target already exists")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect local target: %w", err)
	}
	return localPath, nil
}

func (executor *mcpSyncExecutor) createRemoteDirectory(ctx context.Context, item syncplanpkg.Item) (resultErr error) {
	defer func() { resultErr = executor.markMutationFailure(item, syncjournalpkg.MutationStageWrite, resultErr) }()
	if err := executor.validateLocalDirectorySnapshot(item); err != nil {
		return err
	}
	if err := executor.ensureRemoteAbsent(item.RemotePath); err != nil {
		return err
	}
	parent := syncplanpkg.CanonicalRemoteRoot(pathpkg.Dir(strings.ReplaceAll(item.RemotePath, "\\", "/")))
	parentID, err := executor.resolver().ResolveDir(parent)
	if err != nil {
		return fmt.Errorf("resolve remote parent: %w", err)
	}
	name, err := validateMCPRemoteObjectName(pathpkg.Base(strings.ReplaceAll(item.RemotePath, "\\", "/")))
	if err != nil {
		return err
	}
	createdID, err := executor.ft.client.Mkdir(parentID, name)
	if err != nil {
		return fmt.Errorf("create remote directory: %w", err)
	}
	resolvedID, err := executor.resolver().ResolveDir(item.RemotePath)
	if err != nil || strings.TrimSpace(createdID) == "" || resolvedID != createdID {
		return fmt.Errorf("verify created remote directory")
	}
	return nil
}

func (executor *mcpSyncExecutor) removeRemote(ctx context.Context, item syncplanpkg.Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executor.validateLocalWinnerSnapshot(item); err != nil {
		return fmt.Errorf("validate planned local replacement winner: %w", err)
	}
	if item.ReplacesKind == "directory" {
		if err := executor.validateRemoteSubtree(ctx, item); err != nil {
			return err
		}
	} else if _, err := executor.validateRemoteSnapshot(item, item.ReplacesKind); err != nil {
		return err
	}
	if err := executor.ft.client.Delete(item.RemoteID); err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageRemove, fmt.Errorf("remove planned remote replacement target: %w", err))
	}
	if err := executor.ensureRemoteAbsent(item.RemotePath); err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageRemove, fmt.Errorf("verify removed remote replacement target: %w", err))
	}
	return nil
}

func (executor *mcpSyncExecutor) deleteRemote(ctx context.Context, item syncplanpkg.Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if item.LocalPresent || !item.RemotePresent {
		return fmt.Errorf("planned remote mirror-delete target no longer has remote-only ownership")
	}
	if item.Kind == "directory" {
		if err := executor.validateRemoteSubtree(ctx, item); err != nil {
			return err
		}
	} else if _, err := executor.validateRemoteSnapshot(item, item.Kind); err != nil {
		return err
	}
	if err := executor.ft.client.Delete(item.RemoteID); err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageDelete, fmt.Errorf("delete planned remote mirror target: %w", err))
	}
	if err := executor.ensureRemoteAbsent(item.RemotePath); err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageDelete, fmt.Errorf("verify deleted remote mirror target: %w", err))
	}
	return nil
}

func (executor *mcpSyncExecutor) uploadFile(ctx context.Context, item syncplanpkg.Item) (resultErr error) {
	defer func() { resultErr = executor.markMutationFailure(item, syncjournalpkg.MutationStageWrite, resultErr) }()
	file, digest, err := executor.openLocalFileSnapshot(item)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := executor.ensureRemoteAbsent(item.RemotePath); err != nil {
		return err
	}
	parent := syncplanpkg.CanonicalRemoteRoot(pathpkg.Dir(strings.ReplaceAll(item.RemotePath, "\\", "/")))
	parentID, err := executor.resolver().ResolveDir(parent)
	if err != nil {
		return fmt.Errorf("resolve remote upload parent: %w", err)
	}
	name, err := validateMCPRemoteObjectName(pathpkg.Base(strings.ReplaceAll(item.RemotePath, "\\", "/")))
	if err != nil {
		return err
	}
	_, err = executor.ft.uploadThroughTransferPrepared(ctx, parentID, name, item.LocalSize, file, digest)
	if err != nil {
		return fmt.Errorf("upload planned file: %w", err)
	}
	return nil
}

func (executor *mcpSyncExecutor) createLocalDirectory(ctx context.Context, item syncplanpkg.Item) (resultErr error) {
	defer func() { resultErr = executor.markMutationFailure(item, syncjournalpkg.MutationStageWrite, resultErr) }()
	if _, err := executor.validateRemoteSnapshot(item, "directory"); err != nil {
		return err
	}
	localPath, err := executor.ensureLocalAbsent(item)
	if err != nil {
		return err
	}
	if err := os.Mkdir(localPath, 0o755); err != nil {
		return fmt.Errorf("create local directory: %w", err)
	}
	return nil
}

func (executor *mcpSyncExecutor) removeLocal(ctx context.Context, item syncplanpkg.Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := executor.validateRemoteSnapshot(item, item.Kind); err != nil {
		return fmt.Errorf("validate planned remote replacement winner: %w", err)
	}
	if item.ReplacesKind == "directory" {
		if err := executor.validateLocalSubtree(item); err != nil {
			return err
		}
	} else if err := executor.validateLocalFileSnapshot(item); err != nil {
		return err
	}
	localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, true)
	if err != nil {
		return err
	}
	if item.ReplacesKind == "directory" {
		err = os.RemoveAll(localPath)
	} else {
		err = os.Remove(localPath)
	}
	if err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageRemove, fmt.Errorf("remove planned local replacement target: %w", err))
	}
	if _, err := os.Lstat(localPath); err == nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageRemove, fmt.Errorf("verify removed local replacement target"))
	} else if !os.IsNotExist(err) {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageRemove, fmt.Errorf("verify removed local replacement target: %w", err))
	}
	return nil
}

func (executor *mcpSyncExecutor) deleteLocal(ctx context.Context, item syncplanpkg.Item) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !item.LocalPresent || item.RemotePresent {
		return fmt.Errorf("planned local mirror-delete target no longer has local-only ownership")
	}
	if item.Kind == "directory" {
		if err := executor.validateLocalSubtree(item); err != nil {
			return err
		}
	} else if err := executor.validateLocalFileSnapshot(item); err != nil {
		return err
	}
	localPath, err := validateLocalPath(executor.ft.localRoot, item.LocalPath, true)
	if err != nil {
		return err
	}
	if item.Kind == "directory" {
		err = os.RemoveAll(localPath)
	} else {
		err = os.Remove(localPath)
	}
	if err != nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageDelete, fmt.Errorf("delete planned local mirror target: %w", err))
	}
	if _, err := os.Lstat(localPath); err == nil {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageDelete, fmt.Errorf("verify deleted local mirror target"))
	} else if !os.IsNotExist(err) {
		return executor.markMutationFailure(item, syncjournalpkg.MutationStageDelete, fmt.Errorf("verify deleted local mirror target: %w", err))
	}
	return nil
}

func (executor *mcpSyncExecutor) downloadFile(ctx context.Context, item syncplanpkg.Item) (resultErr error) {
	defer func() { resultErr = executor.markMutationFailure(item, syncjournalpkg.MutationStageWrite, resultErr) }()
	remote, err := executor.validateRemoteSnapshot(item, "file")
	if err != nil {
		return err
	}
	pickCode := strings.TrimSpace(remote.PickCode)
	if pickCode == "" {
		return fmt.Errorf("planned remote file has no download identity")
	}
	localPath, err := executor.ensureLocalAbsent(item)
	if err != nil {
		return err
	}
	info, err := executor.ft.client.DownloadWithUA(pickCode, "")
	if err != nil {
		text := strings.ReplaceAll(err.Error(), pickCode, "[REDACTED_PICK_CODE]")
		return fmt.Errorf("resolve planned download metadata: %s", text)
	}
	if info == nil || int64(info.FileSize) != item.RemoteSize {
		return fmt.Errorf("planned download metadata changed size")
	}
	if executor.ft.downloadTransfer == nil {
		executor.ft.downloadTransfer = newMCPDownloadTransferState()
	}
	config := normalizeDownloadTransferConfig(executor.ft.downloadTransfer.config)
	if _, err := validateMCPDownloadInfoForTransfer(info, executor.ft.downloadMaxBytes, config.Strategy); err != nil {
		return err
	}
	if _, err := executor.ft.downloadThroughTransfer(ctx, info, localPath, pickCode, ""); err != nil {
		return fmt.Errorf("download planned file: %w", err)
	}
	if item.RemoteSHA1 != "" {
		entry := syncplanpkg.Entry{RelativePath: item.RelativePath, Kind: "file", LocalPath: localPath, Size: item.RemoteSize}
		postInfo, statErr := os.Stat(localPath)
		if statErr != nil {
			return fmt.Errorf("verify downloaded file: %w", statErr)
		}
		entry.ModTimeUnixNano = postInfo.ModTime().UnixNano()
		digest, digestErr := syncplanpkg.PrepareLocalDigest(entry)
		if digestErr != nil || digest == nil || !strings.EqualFold(digest.SHA1, item.RemoteSHA1) {
			_ = os.Remove(localPath)
			return fmt.Errorf("downloaded file failed content verification")
		}
	}
	return nil
}

func (executor *mcpSyncExecutor) deps(preflight func(context.Context) error, needsUpload, needsDownload bool) syncexecpkg.Deps {
	fileTransferSlot := make(chan struct{}, 1)
	return syncexecpkg.Deps{
		ForcePreflight: true,
		Preflight:      preflight,
		Prepare: func() error {
			if needsUpload {
				if err := executor.ft.validateUploadTransferReadiness(); err != nil {
					return err
				}
			}
			if needsDownload {
				if executor.ft.downloadTransfer == nil {
					executor.ft.downloadTransfer = newMCPDownloadTransferState()
				}
				if err := normalizeDownloadTransferConfig(executor.ft.downloadTransfer.config).Validate(); err != nil {
					return err
				}
			}
			return nil
		},
		Parallelism: func() (int, int) {
			if needsUpload || needsDownload {
				return 1, 1
			}
			return 0, 0
		},
		AcquireFileTransfer: func(ctx context.Context) (func(), error) {
			select {
			case fileTransferSlot <- struct{}{}:
				return func() { <-fileTransferSlot }, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		CreateRemoteDirectory: executor.createRemoteDirectory,
		RemoveRemote:          executor.removeRemote,
		DeleteRemote:          executor.deleteRemote,
		UploadFile:            executor.uploadFile,
		CreateLocalDirectory:  executor.createLocalDirectory,
		RemoveLocal:           executor.removeLocal,
		DeleteLocal:           executor.deleteLocal,
		DownloadFile:          executor.downloadFile,
	}
}

func executeSyncPlanCallResult(response MCPSyncExecutionOutput) (*mcp.CallToolResult, MCPSyncExecutionOutput, error) {
	isError := response.ErrorCode != "" || response.Summary.Failed > 0 || response.Summary.Blocked > 0
	return mcpTypedJSONResult("execute_sync_plan", response, response, isError)
}

func (ft *FileTools) executePreparedMCPSyncPlan(ctx context.Context, args ExecuteSyncPlanArgs, expectedPlanID string, planArgs PlanSyncArgs, plan syncplanpkg.Plan, jobs int, journalRun *mcpSyncJournalRun, preflight func(context.Context, *mcpSyncExecutor) error) (*mcp.CallToolResult, MCPSyncExecutionOutput, error) {
	if journalRun != nil {
		defer journalRun.close()
		// Match CLI semantics: configured opportunistic session/trash GC is
		// best-effort and never blocks the reviewed sync execution.
		_ = ft.runMCPSessionOpportunisticGC()
	}
	executor := &mcpSyncExecutor{ft: ft, plan: plan}
	barrier := func(preflightCtx context.Context) error {
		if preflight == nil {
			return nil
		}
		return preflight(preflightCtx, executor)
	}
	needsUpload, needsDownload := syncexecpkg.PlanFileTransferNeeds(plan)
	deps := executor.deps(barrier, needsUpload, needsDownload)
	deps = attachMCPSyncJournalDeps(journalRun, executor, deps)
	summary, executionErr := syncexecpkg.ExecuteWithJobsFailurePolicy(ctx, plan, true, jobs, args.ContinueOnError, args.MaxErrors, deps)
	journalRecoveryRequired := executor.recoveryRequired.Load() || errors.Is(executionErr, errMCPSyncRecoveryRequired)
	if journalRun != nil {
		if err := journalRun.finalize(&summary, executionErr, journalRecoveryRequired); err != nil {
			executionErr = errors.Join(executionErr, fmt.Errorf("persist sync journal final state: %w", err))
			journalRecoveryRequired = true
		}
		status := journalRun.handle.Snapshot().Status
		if status == syncjournalpkg.StatusRecoveryRequired || status == syncjournalpkg.StatusReconcileRequired {
			journalRecoveryRequired = true
		}
	}
	output := mcpSyncExecutionOutput(expectedPlanID, summary, ft, planArgs, plan, executionErr)
	if journalRecoveryRequired {
		output.RecoveryRequired = true
		output.ErrorCode = "recovery_required"
		output.Error = "sync execution reached an ambiguous destructive state; inspect and diagnose the persistent sync journal before any retry"
	} else if errors.Is(executionErr, errMCPSyncJournalStateChanged) {
		output.ErrorCode = "journal_state_changed"
		output.Error = "persistent sync journal state changed at the execution barrier; run plan_sync again"
	} else if errors.Is(executionErr, errMCPSyncPlanChanged) {
		output.ErrorCode = "plan_changed"
		output.Error = "sync plan changed at the execution barrier; run plan_sync again"
	} else if errors.Is(executionErr, syncexecpkg.ErrPlanNotReady) {
		output.ErrorCode = "plan_not_ready"
		output.Error = "sync plan is no longer ready; run plan_sync again"
	} else if errors.Is(executionErr, errMCPSyncDeleteBudgetExceeded) {
		output.ErrorCode = "delete_budget_exceeded"
		output.Error = "sync plan exceeds the requested mirror-delete execution budget"
	}
	return executeSyncPlanCallResult(output)
}

func (ft *FileTools) executeSyncPlan(ctx context.Context, req *mcp.CallToolRequest, args ExecuteSyncPlanArgs) (*mcp.CallToolResult, MCPSyncExecutionOutput, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), MCPSyncExecutionOutput{}, nil
	}
	if expectedPlanID == "" {
		return toolError("expect_plan_id is required"), MCPSyncExecutionOutput{}, nil
	}
	jobs, err := normalizeMCPSyncExecutionJobs(args.Jobs)
	if err != nil {
		return toolError(err.Error()), MCPSyncExecutionOutput{}, nil
	}
	if err := syncexecpkg.ValidateFailurePolicy(args.ContinueOnError, args.MaxErrors); err != nil {
		return toolError(err.Error()), MCPSyncExecutionOutput{}, nil
	}
	deleteBudget, err := normalizeMCPSyncDeleteBudget(args)
	if err != nil {
		return toolError(err.Error()), MCPSyncExecutionOutput{}, nil
	}
	planArgs := args.planSyncArgs()

	resumeRun, residual, resumeFound, resumeCode, resumeMessage, resumeErr := ft.prepareMCPSyncResidualResume(ctx, expectedPlanID, planArgs, deleteBudget)
	if resumeErr != nil {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{PlanID: expectedPlanID, ErrorCode: "journal_unavailable", Error: "persistent sync journal could not be prepared for resume"})
	}
	if resumeFound {
		if resumeCode != "" {
			return executeSyncPlanCallResult(MCPSyncExecutionOutput{
				PlanID: expectedPlanID, RecoveryRequired: resumeCode == "recovery_required",
				ErrorCode: resumeCode, Error: resumeMessage,
			})
		}
		return ft.executePreparedMCPSyncPlan(ctx, args, expectedPlanID, planArgs, residual, jobs, resumeRun, func(preflightCtx context.Context, executor *mcpSyncExecutor) error {
			if err := preflightMCPSyncJournalResume(preflightCtx, ft, planArgs, resumeRun.handle.Snapshot()); err != nil {
				return errMCPSyncJournalStateChanged
			}
			executor.plan = residual
			return nil
		})
	}

	state, err := planMCPSyncState(ctx, ft.client, ft.localRoot, planArgs)
	if err != nil {
		return toolError("execute_sync_plan replan failed: " + redactMCPSyncPlanError(err, ft.localRoot, planArgs)), MCPSyncExecutionOutput{}, nil
	}
	if state.Output.Plan.PlanID != expectedPlanID {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{ErrorCode: "plan_changed", Error: "sync plan no longer matches expect_plan_id; run plan_sync again"})
	}
	if !state.Output.Summary.Ready {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{PlanID: expectedPlanID, ErrorCode: "plan_not_ready", Error: "sync plan still has unresolved conflicts"})
	}
	if err := validateMCPSyncExecutablePlan(state.Plan); err != nil {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{PlanID: expectedPlanID, ErrorCode: "invalid_plan", Error: "sync plan is not executable; run plan_sync again"})
	}
	if err := deleteBudget.validate(state.Plan); err != nil {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{PlanID: expectedPlanID, ErrorCode: "delete_budget_exceeded", Error: "reviewed sync plan exceeds the requested mirror-delete execution budget"})
	}

	journalRun, journalCode, journalMessage, journalErr := ft.prepareMCPSyncJournal(ctx, expectedPlanID, state.Plan)
	if journalErr != nil {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{PlanID: expectedPlanID, ErrorCode: "journal_unavailable", Error: "persistent sync journal could not be prepared"})
	}
	if journalCode != "" {
		return executeSyncPlanCallResult(MCPSyncExecutionOutput{
			PlanID: expectedPlanID, RecoveryRequired: journalCode == "recovery_required",
			ErrorCode: journalCode, Error: journalMessage,
		})
	}
	return ft.executePreparedMCPSyncPlan(ctx, args, expectedPlanID, planArgs, state.Plan, jobs, journalRun, func(preflightCtx context.Context, executor *mcpSyncExecutor) error {
		fresh, err := planMCPSyncState(preflightCtx, ft.client, ft.localRoot, planArgs)
		if err != nil {
			return fmt.Errorf("refresh reviewed sync plan: %w", err)
		}
		if fresh.Output.Plan.PlanID != expectedPlanID || fresh.Plan.PlanID != state.Plan.PlanID {
			return errMCPSyncPlanChanged
		}
		if !fresh.Output.Summary.Ready {
			return syncexecpkg.ErrPlanNotReady
		}
		if err := validateMCPSyncExecutablePlan(fresh.Plan); err != nil {
			return err
		}
		if err := deleteBudget.validate(fresh.Plan); err != nil {
			return err
		}
		executor.plan = fresh.Plan
		return nil
	})
}
