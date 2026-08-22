package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
	syncguardpkg "github.com/SheltonZhu/115driver/internal/syncguard"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	errSyncPlanNotReady         = syncexecpkg.ErrPlanNotReady
	errSyncDestructiveApproval  = syncexecpkg.ErrDestructiveApproval
	errSyncExecutionPreparation = syncexecpkg.ErrExecutionPreparation
)

type syncDeleteBudget struct {
	MaxRoots    int
	MaxItems    int
	MaxBytes    int64
	MaxBytesSet bool
}

func resolveSyncDeleteBudget(maxRoots int, maxBytes string) (syncDeleteBudget, error) {
	return resolveSyncDeleteBudgetWithItems(maxRoots, 0, maxBytes)
}

func resolveSyncDeleteBudgetWithItems(maxRoots, maxItems int, maxBytes string) (syncDeleteBudget, error) {
	if maxRoots < 0 {
		return syncDeleteBudget{}, fmt.Errorf("--max-delete-roots must be >= 0")
	}
	if maxItems < 0 {
		return syncDeleteBudget{}, fmt.Errorf("--max-delete-items must be >= 0")
	}
	budget := syncDeleteBudget{MaxRoots: maxRoots, MaxItems: maxItems}
	maxBytes = strings.TrimSpace(maxBytes)
	if maxBytes == "" {
		return budget, nil
	}
	parsed, err := transfer.ParseByteSize(maxBytes)
	if err != nil {
		return syncDeleteBudget{}, fmt.Errorf("invalid --max-delete-bytes %q: %w", maxBytes, err)
	}
	budget.MaxBytes = parsed
	budget.MaxBytesSet = true
	return budget, nil
}

func syncPlanDeleteTotals(plan syncPlan) (roots, items int, bytes int64) {
	return plan.DeleteTotals()
}

func validateSyncDeleteBudget(plan syncPlan, budget syncDeleteBudget) error {
	roots, items, bytes := syncPlanDeleteTotals(plan)
	if budget.MaxRoots > 0 && roots > budget.MaxRoots {
		return fmt.Errorf("sync mirror-delete root budget exceeded: planned %d root(s) > --max-delete-roots %d", roots, budget.MaxRoots)
	}
	if budget.MaxItems > 0 && items > budget.MaxItems {
		return fmt.Errorf("sync mirror-delete item budget exceeded: planned %d affected item(s) > --max-delete-items %d", items, budget.MaxItems)
	}
	if budget.MaxBytesSet && bytes > budget.MaxBytes {
		return fmt.Errorf("sync mirror-delete byte budget exceeded: planned %s > --max-delete-bytes %s", output.FormatFileSize(bytes), output.FormatFileSize(budget.MaxBytes))
	}
	return nil
}

func validateSyncDeleteBudgetUsage(deleteEnabled bool, budget syncDeleteBudget) error {
	if deleteEnabled || (budget.MaxRoots == 0 && budget.MaxItems == 0 && !budget.MaxBytesSet) {
		return nil
	}
	return fmt.Errorf("--max-delete-roots, --max-delete-items, and --max-delete-bytes require --delete")
}

func validateSyncFailurePolicy(continueOnError bool, maxErrors int) error {
	return syncexecpkg.ValidateFailurePolicy(continueOnError, maxErrors)
}

type syncExecutionDeps struct {
	forcePreflight        bool
	beforeItem            func(context.Context, int, syncPlanItem) error
	afterItem             func(context.Context, int, syncPlanItem, syncExecutionItemOutcome) error
	preflight             func(context.Context) error
	prepare               func() error
	parallelism           func() (fileTransferSlots, workersPerTransfer int)
	acquireFileTransfer   func(context.Context) (func(), error)
	createRemoteDirectory func(context.Context, syncPlanItem) error
	removeRemote          func(context.Context, syncPlanItem) error
	deleteRemote          func(context.Context, syncPlanItem) error
	uploadFile            func(context.Context, syncPlanItem) error
	createLocalDirectory  func(context.Context, syncPlanItem) error
	removeLocal           func(context.Context, syncPlanItem) error
	deleteLocal           func(context.Context, syncPlanItem) error
	downloadFile          func(context.Context, syncPlanItem) error
}

func sharedSyncExecutionDeps(deps syncExecutionDeps) syncexecpkg.Deps {
	return syncexecpkg.Deps{
		ForcePreflight:        deps.forcePreflight,
		BeforeItem:            deps.beforeItem,
		AfterItem:             deps.afterItem,
		Preflight:             deps.preflight,
		Prepare:               deps.prepare,
		Parallelism:           deps.parallelism,
		AcquireFileTransfer:   deps.acquireFileTransfer,
		CreateRemoteDirectory: deps.createRemoteDirectory,
		RemoveRemote:          deps.removeRemote,
		DeleteRemote:          deps.deleteRemote,
		UploadFile:            deps.uploadFile,
		CreateLocalDirectory:  deps.createLocalDirectory,
		RemoveLocal:           deps.removeLocal,
		DeleteLocal:           deps.deleteLocal,
		DownloadFile:          deps.downloadFile,
	}
}

type syncExecutionItemResult = syncexecpkg.ItemResult

type syncExecutionSummary = syncexecpkg.Summary

func newSyncExecutionSummary(plan syncPlan, allowDestructive bool) syncExecutionSummary {
	return newSyncExecutionSummaryWithJobs(plan, allowDestructive, 1)
}

func newSyncExecutionSummaryWithJobs(plan syncPlan, allowDestructive bool, jobs int) syncExecutionSummary {
	return syncexecpkg.NewSummary(plan, allowDestructive, jobs)
}

type syncExecutionItemOutcome = syncexecpkg.Outcome

func validateSyncExecutionSafety(plan syncPlan, allowDestructive bool) error {
	return syncexecpkg.ValidateSafety(plan, allowDestructive)
}

func executeSyncPlan(ctx context.Context, plan syncPlan, allowDestructive bool, deps syncExecutionDeps) (syncExecutionSummary, error) {
	return executeSyncPlanWithJobs(ctx, plan, allowDestructive, 1, deps)
}

func executeSyncPlanWithJobs(ctx context.Context, plan syncPlan, allowDestructive bool, requestedJobs int, deps syncExecutionDeps) (syncExecutionSummary, error) {
	return executeSyncPlanWithJobsPolicy(ctx, plan, allowDestructive, requestedJobs, false, deps)
}

func executeSyncPlanWithJobsPolicy(ctx context.Context, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool, deps syncExecutionDeps) (syncExecutionSummary, error) {
	return executeSyncPlanWithJobsFailurePolicy(ctx, plan, allowDestructive, requestedJobs, continueOnError, 0, deps)
}

func executeSyncPlanWithJobsFailurePolicy(ctx context.Context, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int, deps syncExecutionDeps) (syncExecutionSummary, error) {
	return syncexecpkg.ExecuteWithJobsFailurePolicy(ctx, plan, allowDestructive, requestedJobs, continueOnError, maxErrors, sharedSyncExecutionDeps(deps))
}

func blockSyncExecutionDependents(failedIndex int, failedPath string, plan syncPlan, graph syncExecutionGraph, blocked []bool, outcomes []*syncExecutionItemOutcome) int {
	return syncexecpkg.BlockDependents(failedIndex, failedPath, plan, graph, blocked, outcomes)
}

func runSyncPlanItem(ctx context.Context, index int, item syncPlanItem, deps syncExecutionDeps) syncExecutionItemOutcome {
	return syncexecpkg.RunItem(ctx, index, item, sharedSyncExecutionDeps(deps))
}

func finalizeSyncExecutionSummary(summary syncExecutionSummary, outcomes []*syncExecutionItemOutcome) syncExecutionSummary {
	return syncexecpkg.FinalizeSummary(summary, outcomes)
}

func validateSyncExecutionDeps(plan syncPlan, deps syncExecutionDeps) error {
	return syncexecpkg.ValidateDeps(plan, sharedSyncExecutionDeps(deps))
}

type syncProductionExecutor struct {
	plan               syncPlan
	client             *driver.Pan115Client
	jobs               int
	fileTransferSlots  int
	workersPerTransfer int
	transferSlots      chan struct{}
	uploadConfig       auth.TransferConfig
	uploadOptions      uploadpkg.Options
	downloadOptions    downloadCommandOptions
}

func newSyncProductionExecutionDeps(cmd *cobra.Command, plan syncPlan) (syncExecutionDeps, error) {
	return newSyncProductionExecutionDepsWithJobs(cmd, plan, 1)
}

func newSyncProductionExecutionDepsWithJobs(_ *cobra.Command, plan syncPlan, requestedJobs int) (syncExecutionDeps, error) {
	if client == nil {
		return syncExecutionDeps{}, errors.New("sync client is nil")
	}
	jobs, err := resolveSyncJobs(requestedJobs)
	if err != nil {
		return syncExecutionDeps{}, err
	}
	executor := &syncProductionExecutor{plan: plan, client: client, jobs: jobs}
	return syncExecutionDeps{
		preflight:             func(ctx context.Context) error { return preflightSyncPlan(ctx, client, plan) },
		prepare:               executor.prepare,
		parallelism:           executor.parallelism,
		acquireFileTransfer:   executor.acquireFileTransfer,
		createRemoteDirectory: executor.createRemoteDirectory,
		removeRemote:          executor.removeRemote,
		deleteRemote:          executor.deleteRemote,
		uploadFile:            executor.uploadFile,
		createLocalDirectory:  executor.createLocalDirectory,
		removeLocal:           executor.removeLocal,
		deleteLocal:           executor.deleteLocal,
		downloadFile:          executor.downloadFile,
	}, nil
}

func syncPlanFileTransferNeeds(plan syncPlan) (upload, download bool) {
	return syncexecpkg.PlanFileTransferNeeds(plan)
}

func syncPlanHasWrites(plan syncPlan) bool {
	return syncexecpkg.PlanHasWrites(plan)
}

func (executor *syncProductionExecutor) prepare() error {
	needsUpload, needsDownload := syncPlanFileTransferNeeds(executor.plan)
	if !needsUpload && !needsDownload {
		return nil
	}
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return err
	}
	workerLimit, parallelTransfers, err := syncTransferBudget(config.WorkersPerInterface, executor.jobs, syncPlanFileTransferCount(executor.plan))
	if err != nil {
		return err
	}
	config.WorkersPerInterface = workerLimit
	executor.workersPerTransfer = workerLimit
	executor.fileTransferSlots = parallelTransfers
	if parallelTransfers > 0 {
		executor.transferSlots = make(chan struct{}, parallelTransfers)
	}
	if needsUpload {
		if ok, err := executor.client.UploadAvailable(); err != nil || !ok {
			if err != nil {
				return fmt.Errorf("prepare 115 upload metadata: %w", err)
			}
			return errors.New("prepare 115 upload metadata: upload is unavailable")
		}
		uploadOptions, err := buildUploadOptions(config, "", "", uploadpkg.DefaultTimeout)
		if err != nil {
			return err
		}
		executor.uploadConfig = config
		executor.uploadOptions = uploadOptions
	}
	if needsDownload {
		downloadOptions, err := syncDownloadOptionsFromConfig(config)
		if err != nil {
			return err
		}
		executor.downloadOptions = downloadOptions
	}
	return nil
}

func (executor *syncProductionExecutor) parallelism() (int, int) {
	return executor.fileTransferSlots, executor.workersPerTransfer
}

func (executor *syncProductionExecutor) acquireFileTransfer(ctx context.Context) (func(), error) {
	if executor.transferSlots == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case executor.transferSlots <- struct{}{}:
		return func() { <-executor.transferSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func preflightSyncPlan(ctx context.Context, planClient syncPlanClient, plan syncPlan) error {
	if !syncPlanHasWrites(plan) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if planClient == nil {
		return errors.New("sync preflight client is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSyncPreflightRoots(planClient, plan); err != nil {
		return err
	}
	for _, item := range plan.Items {
		if err := ensureSyncLocalPathWithinRoot(plan.LocalRoot, item.LocalPath); err != nil {
			return err
		}
		if err := ensureSyncRemotePathWithinRoot(plan.RemoteRoot, item.RemotePath); err != nil {
			return err
		}
	}

	localTree, err := scanLocalUploadTree(plan.LocalRoot)
	if err != nil {
		return fmt.Errorf("rescan local sync tree: %w", err)
	}
	currentLocal, err := collectLocalSyncEntries(localTree, plan.RemoteRoot)
	if err != nil {
		return fmt.Errorf("inspect current local sync tree: %w", err)
	}
	if err := compareSyncPreflightLocalTree(plan, currentLocal); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentRemote, err := collectRemoteSyncEntries(planClient, plan.RemoteRootID, plan.RemoteRoot, plan.LocalRoot)
	if err != nil {
		return fmt.Errorf("rescan remote sync tree: %w", err)
	}
	if err := compareSyncPreflightRemoteTree(planClient, plan, currentRemote); err != nil {
		return err
	}
	return ctx.Err()
}

func validateSyncPreflightRoots(planClient syncPlanClient, plan syncPlan) error {
	localInfo, err := os.Lstat(plan.LocalRoot)
	if err != nil {
		return fmt.Errorf("local sync root %q changed after planning: %w", plan.LocalRoot, err)
	}
	if localInfo.Mode()&os.ModeSymlink != 0 || !localInfo.IsDir() {
		return fmt.Errorf("local sync root %q is no longer a real directory", plan.LocalRoot)
	}
	remoteID, err := resolver.ResolveDir(planClient, plan.RemoteRoot)
	if err != nil {
		return fmt.Errorf("remote sync root %q changed after planning: %w", plan.RemoteRoot, err)
	}
	if strings.TrimSpace(plan.RemoteRootID) == "" || remoteID != plan.RemoteRootID {
		return fmt.Errorf("remote sync root %q changed identity after planning: expected %q got %q", plan.RemoteRoot, plan.RemoteRootID, remoteID)
	}
	return nil
}

func compareSyncPreflightLocalTree(plan syncPlan, current map[string]syncTreeEntry) error {
	return syncjournalpkg.CompareExpectedLocalTree(plan, current)
}

func compareSyncPreflightRemoteTree(planClient syncPlanClient, plan syncPlan, current map[string]syncTreeEntry) error {
	return syncjournalpkg.CompareExpectedRemoteTree(plan, current, func(entry syncTreeEntry) (string, error) {
		return resolveSyncRemoteSHA1(planClient, entry)
	})
}

func syncDownloadOptionsFromConfig(config auth.TransferConfig) (downloadCommandOptions, error) {
	chunkSize, err := transfer.ParseByteSize(config.ChunkSize)
	if err != nil {
		return downloadCommandOptions{}, fmt.Errorf("invalid transfer chunk size %q: %v", config.ChunkSize, err)
	}
	options := downloadCommandOptions{
		Recursive: false, Timeout: defaultDownloadTimeout, Interfaces: config.Interfaces, Strategy: config.Strategy,
		WorkersPerInterface: config.WorkersPerInterface, ProbeCacheTTL: config.ProbeCacheTTL, Retries: config.Retries,
		ChunkSize: chunkSize, HealthCooldown: config.HealthCooldown, HealthCooldownMax: config.HealthCooldownMax,
		Resume: config.Resume, URLRefreshes: config.URLRefreshes,
	}
	if err := validateDownloadCommandOptions(options); err != nil {
		return downloadCommandOptions{}, err
	}
	return options, nil
}

func (executor *syncProductionExecutor) createRemoteDirectory(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executor.validateLocalWinner(item); err != nil {
		return err
	}
	if err := syncEnsureRemoteAbsent(executor.client, item.RemotePath); err != nil {
		return err
	}
	parentPath := pathpkg.Dir(item.RemotePath)
	parentID, err := resolver.ResolveDir(executor.client, parentPath)
	if err != nil {
		return fmt.Errorf("resolve remote parent %q: %w", parentPath, err)
	}
	name := pathpkg.Base(item.RemotePath)
	if _, err := executor.client.Mkdir(parentID, name); err != nil {
		return fmt.Errorf("create remote sync directory %q: %w", item.RemotePath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) createLocalDirectory(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executor.validateRemoteWinner(item); err != nil {
		return err
	}
	if err := ensureSyncLocalPathWithinRoot(executor.plan.LocalRoot, item.LocalPath); err != nil {
		return err
	}
	if err := syncEnsureLocalAbsent(item.LocalPath); err != nil {
		return err
	}
	if err := os.Mkdir(item.LocalPath, 0755); err != nil {
		return fmt.Errorf("create local sync directory %q: %w", item.LocalPath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) removeRemote(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureSyncRemotePathWithinRoot(executor.plan.RemoteRoot, item.RemotePath); err != nil {
		return err
	}
	if err := executor.validateLocalWinner(item); err != nil {
		return err
	}
	if err := syncValidateRemoteSnapshot(executor.client, item, item.ReplacesKind); err != nil {
		return err
	}
	if item.ReplacesKind == "directory" {
		if err := syncValidateRemoteReplacementSubtree(executor.client, executor.plan, item); err != nil {
			return err
		}
	}
	if err := executor.client.Delete(item.RemoteID); err != nil {
		return fmt.Errorf("remove planned remote replacement target %q: %w", item.RemotePath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) removeLocal(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureSyncLocalPathWithinRoot(executor.plan.LocalRoot, item.LocalPath); err != nil {
		return err
	}
	if err := syncValidateLocalSnapshot(item, item.ReplacesKind); err != nil {
		return err
	}
	if item.ReplacesKind == "directory" {
		if err := syncValidateLocalReplacementSubtree(executor.plan, item); err != nil {
			return err
		}
	}
	if err := executor.validateRemoteWinner(item); err != nil {
		return err
	}
	if err := os.RemoveAll(item.LocalPath); err != nil {
		return fmt.Errorf("remove planned local replacement target %q: %w", item.LocalPath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) deleteRemote(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureSyncRemotePathWithinRoot(executor.plan.RemoteRoot, item.RemotePath); err != nil {
		return err
	}
	if item.LocalPresent || !item.RemotePresent {
		return fmt.Errorf("sync mirror delete %q no longer has remote-only ownership", item.RelativePath)
	}
	if err := syncValidateRemoteSnapshot(executor.client, item, item.Kind); err != nil {
		return err
	}
	if item.Kind == "directory" {
		if err := syncValidateRemoteReplacementSubtree(executor.client, executor.plan, item); err != nil {
			return err
		}
	}
	if err := executor.client.Delete(item.RemoteID); err != nil {
		return fmt.Errorf("delete planned remote mirror target %q: %w", item.RemotePath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) deleteLocal(ctx context.Context, item syncPlanItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureSyncLocalPathWithinRoot(executor.plan.LocalRoot, item.LocalPath); err != nil {
		return err
	}
	if !item.LocalPresent || item.RemotePresent {
		return fmt.Errorf("sync mirror delete %q no longer has local-only ownership", item.RelativePath)
	}
	if err := syncValidateLocalSnapshot(item, item.Kind); err != nil {
		return err
	}
	if item.Kind == "directory" {
		if err := syncValidateLocalReplacementSubtree(executor.plan, item); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(item.LocalPath); err != nil {
		return fmt.Errorf("delete planned local mirror target %q: %w", item.LocalPath, err)
	}
	return nil
}

func (executor *syncProductionExecutor) configureUploadProgress(options *uploadpkg.Options, sourceLabel string) func() {
	if executor.jobs <= 1 {
		return configureCLIUploadProgress(options, sourceLabel)
	}
	if options == nil || jsonOutput {
		return func() {}
	}
	options.ProgressBytes = nil
	options.Progress = func(message string) {
		if importantUploadStatus(message) {
			fmt.Fprintf(os.Stderr, "[%s] %s\n", sourceLabel, message)
		}
	}
	return func() {}
}

func (executor *syncProductionExecutor) uploadFile(ctx context.Context, item syncPlanItem) error {
	if err := executor.validateLocalWinner(item); err != nil {
		return err
	}
	if err := syncEnsureRemoteAbsent(executor.client, item.RemotePath); err != nil {
		return err
	}
	parentPath := pathpkg.Dir(item.RemotePath)
	dirID, err := resolver.ResolveDir(executor.client, parentPath)
	if err != nil {
		return fmt.Errorf("resolve remote upload parent %q: %w", parentPath, err)
	}
	stat, err := os.Lstat(item.LocalPath)
	if err != nil {
		return fmt.Errorf("stat sync upload source %q: %w", item.LocalPath, err)
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		return fmt.Errorf("sync upload source %q is no longer a regular file", item.LocalPath)
	}
	options := executor.uploadOptions
	options.ResumePath = ""
	options.PreparedDigest = item.LocalPreparedDigest
	finishProgress := executor.configureUploadProgress(&options, item.RelativePath)
	defer finishProgress()

	var sessionResolution transferSessionResolution
	if executor.uploadConfig.Resume {
		sessionResolution, err = resolveTransferSessionPaths("upload", "file", item.LocalPath, parentPath, "multipart", "single-file", "")
		if err != nil {
			return err
		}
		options.ResumePath = sessionResolution.SessionPath
		defer sessionResolution.closeLock()
	}
	file, err := os.Open(item.LocalPath)
	if err != nil {
		return fmt.Errorf("open sync upload source %q: %w", item.LocalPath, err)
	}
	defer file.Close()
	fileName := pathpkg.Base(item.RemotePath)
	if executor.uploadConfig.Resume {
		needsLegacyImport, err := legacyTransferSessionImportNeeded(sessionResolution)
		if err != nil {
			return fmt.Errorf("inspect legacy sync upload session: %w", err)
		}
		if needsLegacyImport {
			digest := options.PreparedDigest
			if digest == nil {
				digest, err = uploadpkg.PrepareFileDigest(file, stat.Size())
				if err != nil {
					return fmt.Errorf("prepare sync upload resume identity: %w", err)
				}
				options.PreparedDigest = digest
			}
			if err := importLegacyTransferSession(&sessionResolution, func(path string) (bool, error) {
				return uploadpkg.ValidateResumeStateIdentity(path, dirID, fileName, stat.Size(), digest.SHA1)
			}); err != nil {
				return fmt.Errorf("import legacy sync upload session: %w", err)
			}
		}
	}
	_, err = uploadpkg.UploadFile(ctx, executor.client, dirID, fileName, stat.Size(), file, options)
	if err == nil {
		commitLegacyTransferSessionImport(sessionResolution)
	}
	if err != nil {
		return err
	}
	if options.ResumePath != "" {
		if err := uploadpkg.RemoveResumeState(options.ResumePath); err != nil && options.Progress != nil {
			options.Progress(fmt.Sprintf("Warning: upload succeeded but resume state cleanup failed: %v", err))
		}
	}
	if err := cleanupResolvedTransferSession(sessionResolution); err != nil {
		return fmt.Errorf("sync upload succeeded but managed session cleanup failed: %w", err)
	}
	return nil
}

func (executor *syncProductionExecutor) downloadFile(ctx context.Context, item syncPlanItem) error {
	if err := executor.validateRemoteWinner(item); err != nil {
		return err
	}
	if err := ensureSyncLocalPathWithinRoot(executor.plan.LocalRoot, item.LocalPath); err != nil {
		return err
	}
	if err := syncEnsureLocalAbsent(item.LocalPath); err != nil {
		return err
	}
	options := executor.downloadOptions
	options.Recursive = false
	options.SessionPath = ""
	if !jsonOutput {
		options.Progress = func(message string) {
			fmt.Fprintf(os.Stderr, "[%s] %s\n", item.RelativePath, message)
		}
	}
	summary, err := executeDownloadCommand(ctx, executor.client, item.RemotePath, item.LocalPath, options, defaultDownloadPipelineDeps())
	if err != nil {
		return err
	}
	if summary.SucceededCount != 1 || summary.FailedCount != 0 {
		return fmt.Errorf("sync download of %q completed with succeeded=%d failed=%d", item.RemotePath, summary.SucceededCount, summary.FailedCount)
	}
	return nil
}

func (executor *syncProductionExecutor) validateLocalWinner(item syncPlanItem) error {
	if !item.LocalPresent {
		return fmt.Errorf("sync plan item %q has no local winner", item.RelativePath)
	}
	return syncValidateLocalSnapshot(item, item.Kind)
}

func (executor *syncProductionExecutor) validateRemoteWinner(item syncPlanItem) error {
	if !item.RemotePresent {
		return fmt.Errorf("sync plan item %q has no remote winner", item.RelativePath)
	}
	return syncValidateRemoteSnapshot(executor.client, item, item.Kind)
}

func syncValidateLocalSnapshot(item syncPlanItem, expectedKind string) error {
	info, err := os.Lstat(item.LocalPath)
	if err != nil {
		return fmt.Errorf("local sync path %q changed after planning: %w", item.LocalPath, err)
	}
	actualKind := "file"
	if info.IsDir() {
		actualKind = "directory"
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("local sync path %q changed to an unsupported file type", item.LocalPath)
	}
	if actualKind != expectedKind {
		return fmt.Errorf("local sync path %q changed type after planning: expected %s got %s", item.LocalPath, expectedKind, actualKind)
	}
	if actualKind == "file" && info.Size() != item.LocalSize {
		return fmt.Errorf("local sync file %q changed size after planning: expected %d got %d", item.LocalPath, item.LocalSize, info.Size())
	}
	if item.LocalModTimeUnixNano != 0 && info.ModTime().UnixNano() != item.LocalModTimeUnixNano {
		return fmt.Errorf("local sync path %q changed modification time after planning", item.LocalPath)
	}
	return nil
}

func syncValidateRemoteSnapshot(client syncPlanClient, item syncPlanItem, expectedKind string) error {
	remoteID, isDirectory, err := resolver.ResolvePath(client, item.RemotePath)
	if err != nil {
		return fmt.Errorf("remote sync path %q changed after planning: %w", item.RemotePath, err)
	}
	actualKind := "file"
	if isDirectory {
		actualKind = "directory"
	}
	if actualKind != expectedKind {
		return fmt.Errorf("remote sync path %q changed type after planning: expected %s got %s", item.RemotePath, expectedKind, actualKind)
	}
	if strings.TrimSpace(item.RemoteID) == "" || remoteID != item.RemoteID {
		return fmt.Errorf("remote sync path %q changed identity after planning: expected %q got %q", item.RemotePath, item.RemoteID, remoteID)
	}
	if actualKind == "directory" {
		return nil
	}
	file, err := client.GetFile(remoteID)
	if err != nil {
		return fmt.Errorf("inspect remote sync file %q before execution: %w", item.RemotePath, err)
	}
	if file == nil || file.IsDirectory || file.Size != item.RemoteSize {
		return fmt.Errorf("remote sync file %q changed metadata after planning", item.RemotePath)
	}
	if item.RemoteSHA1 != "" && !strings.EqualFold(strings.TrimSpace(file.Sha1), item.RemoteSHA1) {
		return fmt.Errorf("remote sync file %q changed SHA1 after planning", item.RemotePath)
	}
	return nil
}

func syncValidateRemoteReplacementSubtree(client remoteTreeListClient, plan syncPlan, rootItem syncPlanItem) error {
	return syncguardpkg.ValidateRemoteSubtree(client, plan, rootItem)
}

func syncValidateLocalReplacementSubtree(plan syncPlan, rootItem syncPlanItem) error {
	return syncguardpkg.ValidateLocalSubtree(plan, rootItem)
}

func syncEnsureRemoteAbsent(client remotePathResolveClient, remotePath string) error {
	parentPath := pathpkg.Dir(remotePath)
	parentID, err := resolver.ResolveDir(client, parentPath)
	if err != nil {
		return fmt.Errorf("resolve remote parent %q: %w", parentPath, err)
	}
	entries, err := listRemoteDirectoryReadOnly(client, parentID)
	if err != nil {
		return fmt.Errorf("inspect remote parent %q: %w", parentPath, err)
	}
	if entries == nil {
		return fmt.Errorf("inspect remote parent %q: empty listing response", parentPath)
	}
	name := pathpkg.Base(remotePath)
	for _, entry := range *entries {
		if entry.Name == name {
			return fmt.Errorf("remote sync path %q appeared after planning; refusing stale plan", remotePath)
		}
	}
	return nil
}

func syncEnsureLocalAbsent(localPath string) error {
	_, err := os.Lstat(localPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect local sync target %q: %w", localPath, err)
	}
	return fmt.Errorf("local sync path %q appeared after planning; refusing stale plan", localPath)
}

func ensureSyncLocalPathWithinRoot(localRoot, localPath string) error {
	root, err := filepath.Abs(localRoot)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(localPath)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if relative == "." || relative == "" || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("sync local target %q escapes or equals root %q", localPath, localRoot)
	}
	return nil
}

func ensureSyncRemotePathWithinRoot(remoteRoot, remotePath string) error {
	root := canonicalSyncRemoteRoot(remoteRoot)
	target := canonicalSyncRemoteRoot(remotePath)
	if target == root {
		return fmt.Errorf("sync remote target %q equals sync root", remotePath)
	}
	prefix := strings.TrimRight(root, "/") + "/"
	if root == "/" {
		prefix = "/"
	}
	if !strings.HasPrefix(target, prefix) {
		return fmt.Errorf("sync remote target %q escapes root %q", remotePath, remoteRoot)
	}
	return nil
}

func printSyncExecutionSummary(summary syncExecutionSummary) {
	if jsonOutput {
		return
	}
	fmt.Printf("Sync complete (plan=%s; journal=%t; journal-state=%s; resumed=%t; already-completed=%d; jobs=%d; continue-on-error=%t; max-errors=%d; file transfer slots=%d; workers/transfer=%d; preflight passed; %d planned item(s) checked): %d succeeded, %d skipped, %d failed, %d blocked; uploaded %d file(s), created %d remote dir(s), downloaded %d file(s), created %d local dir(s), replaced %d remote and %d local target(s), deleted %d remote and %d local mirror root(s).\n",
		summary.PlanID, summary.JournalEnabled, summary.JournalState, summary.JournalResumed, summary.JournalCompletedBefore, summary.Jobs, summary.ContinueOnError, summary.MaxErrors, summary.FileTransferSlots, summary.WorkersPerTransfer, summary.PreflightChecked, summary.Succeeded, summary.Skipped, summary.Failed, summary.Blocked, summary.UploadedFiles, summary.CreatedRemoteDirs,
		summary.DownloadedFiles, summary.CreatedLocalDirs, summary.ReplacedRemote, summary.ReplacedLocal, summary.DeletedRemote, summary.DeletedLocal)
}

func syncExecutionErrorCode(err error) int {
	code := classifyRemoteError(err, output.ExitError)
	if errors.Is(err, errSyncPlanNotReady) || errors.Is(err, errSyncDestructiveApproval) {
		return output.ExitArgs
	}
	if errors.Is(err, errSyncExecutionPreparation) && code != output.ExitNetwork {
		return output.ExitArgs
	}
	return code
}

func runSyncExecution(cmd *cobra.Command, plan syncPlan, allowDestructive bool, requestedJobs int) error {
	return runSyncExecutionWithPolicy(cmd, plan, allowDestructive, requestedJobs, false)
}

func runSyncExecutionWithPolicy(cmd *cobra.Command, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool) error {
	return runSyncExecutionWithFailurePolicy(cmd, plan, allowDestructive, requestedJobs, continueOnError, 0)
}

func runSyncExecutionWithFailurePolicy(cmd *cobra.Command, plan syncPlan, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int) error {
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
	deps, err := newSyncProductionExecutionDepsWithJobs(cmd, plan, jobs)
	if err != nil {
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("initialize sync execution: %v", err)}
	}
	summary, err := executeSyncPlanWithJobsFailurePolicy(cmd.Context(), plan, allowDestructive, jobs, continueOnError, maxErrors, deps)
	if err != nil {
		return &exitError{code: syncExecutionErrorCode(err), msg: err.Error(), data: summary}
	}
	printer.PrintSuccess(summary)
	printSyncExecutionSummary(summary)
	return nil
}
