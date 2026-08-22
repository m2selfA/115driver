package cmd

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type remoteMoveCopyPlanItem struct {
	Path        string `json:"path"`
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Destination string `json:"destination"`
}

type remoteMoveCopyPlan struct {
	Operation       string                   `json:"operation"`
	DryRun          bool                     `json:"dry_run"`
	DestinationDir  string                   `json:"destination_dir"`
	DestinationID   string                   `json:"destination_id"`
	InputCount      int                      `json:"input_count"`
	UniqueItemCount int                      `json:"unique_item_count"`
	Items           []remoteMoveCopyPlanItem `json:"items"`
}

func buildMoveOrCopyPlan(action string, resolveClient remotePathResolveClient, srcPaths []string, dstDir string) (remoteMoveCopyPlan, error) {
	plan := remoteMoveCopyPlan{Operation: action, DryRun: true, DestinationDir: dstDir, InputCount: len(srcPaths)}
	if len(srcPaths) == 0 {
		return plan, &exitError{code: output.ExitArgs, msg: "at least one source path is required"}
	}
	pathResolver := resolver.New(resolveClient)
	dirID, err := pathResolver.ResolveDir(dstDir)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve destination directory %s: %v", dstDir, err)}
	}
	plan.DestinationID = dirID
	items, err := resolveUniqueRemoteItemsWithResolver(pathResolver, srcPaths)
	if err != nil {
		return plan, err
	}
	if err := validateRemoteMutationItems(items); err != nil {
		return plan, err
	}
	plan.Items = make([]remoteMoveCopyPlanItem, 0, len(items))
	for _, item := range items {
		if item.ID == dirID {
			return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("source %q is the destination directory", item.Path)}
		}
		kind := "file"
		if item.IsDir {
			kind = "directory"
		}
		base := pathpkg.Base(strings.TrimRight(item.Path, "/"))
		destination := pathpkg.Join(dstDir, base)
		if strings.HasPrefix(dstDir, "/") && !strings.HasPrefix(destination, "/") {
			destination = "/" + destination
		}
		plan.Items = append(plan.Items, remoteMoveCopyPlanItem{Path: item.Path, ID: item.ID, Kind: kind, Destination: destination})
	}
	plan.UniqueItemCount = len(plan.Items)
	return plan, nil
}

func printMoveOrCopyPlan(plan remoteMoveCopyPlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN %s: %d unique item(s) -> %s; no remote data was changed.\n", plan.Operation, plan.UniqueItemCount, plan.DestinationDir)
	for _, item := range plan.Items {
		fmt.Printf("  %s %s (%s) -> %s\n", item.Kind, item.Path, item.ID, item.Destination)
	}
}

type remoteDeletePlanItem struct {
	Path        string `json:"path"`
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Files       int64  `json:"files"`
	Directories int64  `json:"directories"`
	Size        int64  `json:"size"`
	Subtree     bool   `json:"subtree"`
}

type remoteDeletePlan struct {
	Operation string                 `json:"operation"`
	DryRun    bool                   `json:"dry_run"`
	Items     []remoteDeletePlanItem `json:"items"`
}

type remoteDeletePlanClient interface {
	remotePathResolveClient
	GetFile(fileID string) (*driver.File, error)
}

func buildRemoteDeletePlan(planClient remoteDeletePlanClient, paths []string) (remoteDeletePlan, error) {
	plan := remoteDeletePlan{Operation: "delete", DryRun: true}
	items, err := resolveUniqueRemoteItems(planClient, paths)
	if err != nil {
		return plan, err
	}
	if err := validateRemoteMutationItems(items); err != nil {
		return plan, err
	}
	return buildRemoteDeletePlanFromItems(planClient, items)
}

func buildRemoteDeletePlanFromItems(planClient remoteDeletePlanClient, items []resolvedRemoteItem) (remoteDeletePlan, error) {
	plan := remoteDeletePlan{Operation: "delete", DryRun: true}
	plan.Items = make([]remoteDeletePlanItem, 0, len(items))
	for _, item := range items {
		usage, err := summarizeRemoteUsage(planClient, item.Path, 0)
		if err != nil {
			return plan, err
		}
		kind := "file"
		directories := usage.Directories
		if item.IsDir {
			kind = "directory"
			directories++ // include the selected directory itself
		}
		plan.Items = append(plan.Items, remoteDeletePlanItem{
			Path: item.Path, ID: item.ID, Kind: kind, Files: usage.Files, Directories: directories, Size: usage.Size, Subtree: item.IsDir,
		})
	}
	return plan, nil
}

func printRemoteDeletePlan(plan remoteDeletePlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN delete: %d unique item(s); nothing was moved to the recycle bin.\n", len(plan.Items))
	for _, item := range plan.Items {
		if item.Subtree {
			fmt.Printf("  directory %s (%s): subtree %d file(s), %d dir(s), %s\n", item.Path, item.ID, item.Files, item.Directories, output.FormatFileSize(item.Size))
		} else {
			fmt.Printf("  file %s (%s): %s\n", item.Path, item.ID, output.FormatFileSize(item.Size))
		}
	}
}

type mkdirPlan struct {
	Operation  string   `json:"operation"`
	DryRun     bool     `json:"dry_run"`
	Path       string   `json:"path"`
	Parents    bool     `json:"parents"`
	Action     string   `json:"action"`
	ParentPath string   `json:"parent_path,omitempty"`
	Create     []string `json:"create"`
	Reuse      []string `json:"reuse"`
}

func buildMkdirPlan(planClient remotePathResolveClient, remotePath string, parents bool) (mkdirPlan, error) {
	plan := mkdirPlan{Operation: "mkdir", DryRun: true, Parents: parents}
	cleaned := strings.TrimRight(remotePath, "/")
	if cleaned == "" {
		return plan, &exitError{code: output.ExitArgs, msg: "Cannot create root directory."}
	}
	canonical := "/" + strings.Trim(pathpkg.Clean(cleaned), "/")
	if canonical == "/" {
		return plan, &exitError{code: output.ExitArgs, msg: "Cannot create root directory."}
	}
	plan.Path = canonical
	if !parents {
		name := pathpkg.Base(canonical)
		parentPath := pathpkg.Dir(canonical)
		if parentPath == "." {
			parentPath = "/"
		}
		plan.ParentPath = parentPath
		parentID, err := resolver.ResolveDir(planClient, parentPath)
		if err != nil {
			return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve parent directory %s: %v", parentPath, err)}
		}
		entries, err := listRemoteDirectoryReadOnly(planClient, parentID)
		if err != nil {
			return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot inspect parent directory %s: %v", parentPath, err)}
		}
		if entries == nil {
			return plan, &exitError{code: output.ExitError, msg: fmt.Sprintf("Cannot inspect parent directory %s: empty listing response", parentPath)}
		}
		for _, entry := range *entries {
			if entry.Name != name {
				continue
			}
			if entry.IsDirectory {
				return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("directory already exists: %s", canonical)}
			}
			return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("remote file conflicts with directory path: %s", canonical)}
		}
		plan.Action = "create"
		plan.Create = []string{canonical}
		return plan, nil
	}

	parts := strings.Split(strings.Trim(canonical, "/"), "/")
	currentID := resolver.RootID
	currentPath := ""
	creating := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		currentPath += "/" + part
		if creating {
			plan.Create = append(plan.Create, currentPath)
			continue
		}
		existingID, err := resolver.ResolveDir(planClient, currentPath)
		if err == nil && existingID != "" {
			plan.Reuse = append(plan.Reuse, currentPath)
			currentID = existingID
			continue
		}
		if err != nil && !errors.Is(err, driver.ErrNotExist) {
			return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot inspect directory %s: %v", currentPath, err)}
		}
		entries, listErr := listRemoteDirectoryReadOnly(planClient, currentID)
		if listErr != nil {
			return plan, &exitError{code: classifyRemoteError(listErr, output.ExitError), msg: fmt.Sprintf("Cannot inspect parent while planning %s: %v", currentPath, listErr)}
		}
		if entries == nil {
			return plan, &exitError{code: output.ExitError, msg: fmt.Sprintf("Cannot inspect parent while planning %s: empty listing response", currentPath)}
		}
		foundDirectory := false
		for _, entry := range *entries {
			if entry.Name != part {
				continue
			}
			if !entry.IsDirectory {
				return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("remote file conflicts with directory path: %s", currentPath)}
			}
			plan.Reuse = append(plan.Reuse, currentPath)
			currentID = entry.FileID
			foundDirectory = true
			break
		}
		if foundDirectory {
			continue
		}
		creating = true
		plan.Create = append(plan.Create, currentPath)
	}
	if len(plan.Create) == 0 {
		plan.Action = "reuse"
	} else {
		plan.Action = "create"
	}
	return plan, nil
}

func runMkdirDryRun(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		plan, err := buildMkdirPlan(client, args[0], mkdirParents)
		if err != nil {
			return err
		}
		printer.PrintSuccess(plan)
		if !jsonOutput {
			fmt.Printf("DRY-RUN mkdir %s: %d create, %d reuse; no directory was created.\n", plan.Path, len(plan.Create), len(plan.Reuse))
		}
		return nil
	}
	continueOnError := batchContinueOnError(cmd)
	plans := make([]mkdirPlan, 0, len(args))
	items := make([]batchItemResult, 0, len(args))
	for i, remotePath := range args {
		plan, err := buildMkdirPlan(client, remotePath, mkdirParents)
		if err != nil {
			items = append(items, failedBatchItem(remotePath, map[string]interface{}{"dry_run": true, "path": remotePath}, err))
			printBatchItemFailure(i, len(args), "plan mkdir "+remotePath, err)
			if !continueOnError {
				break
			}
			continue
		}
		plans = append(plans, plan)
		items = append(items, successfulBatchItem(remotePath, plan))
		if !jsonOutput {
			fmt.Printf("DRY-RUN mkdir %s: %d create, %d reuse; no directory was created.\n", plan.Path, len(plan.Create), len(plan.Reuse))
		}
	}
	data := batchResultData(len(args), items, map[string]interface{}{"dry_run": true, "operation": "mkdir", "plans": plans})
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("mkdir dry-run", len(args), items, data)
	}
	printer.PrintSuccess(data)
	return nil
}

type offlineSaveDirectoryPlan struct {
	Path string `json:"path,omitempty"`
	ID   string `json:"id"`
}

func resolveOfflineSaveDirectory(resolveClient remotePathResolveClient) (offlineSaveDirectoryPlan, error) {
	plan := offlineSaveDirectoryPlan{ID: resolver.RootID}
	if offlineSaveDir != "" {
		id, err := resolver.ResolveDir(resolveClient, offlineSaveDir)
		if err != nil {
			return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve save directory %s: %v", offlineSaveDir, err)}
		}
		plan.ID, plan.Path = id, offlineSaveDir
		return plan, nil
	}
	if cfgDir := auth.ReadProfileConfig(configPath, profile, "default_offline_save_dir"); cfgDir != "" {
		id, err := resolver.ResolveDir(resolveClient, cfgDir)
		if err != nil {
			return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve save directory %s from config default_offline_save_dir: %v", cfgDir, err)}
		}
		plan.ID, plan.Path = id, cfgDir
	}
	return plan, nil
}

type offlineAddPlan struct {
	Operation        string                   `json:"operation"`
	DryRun           bool                     `json:"dry_run"`
	URLs             []string                 `json:"urls"`
	SaveDirectory    offlineSaveDirectoryPlan `json:"save_directory"`
	ServerValidation string                   `json:"server_validation"`
}

func buildOfflineAddPlan(resolveClient remotePathResolveClient, urls []string) (offlineAddPlan, error) {
	plan := offlineAddPlan{Operation: "offline-add", DryRun: true, URLs: append([]string(nil), urls...), ServerValidation: "deferred-until-submit"}
	for _, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			return plan, &exitError{code: output.ExitArgs, msg: "offline task URL must not be empty"}
		}
	}
	saveDir, err := resolveOfflineSaveDirectory(resolveClient)
	if err != nil {
		return plan, err
	}
	plan.SaveDirectory = saveDir
	return plan, nil
}

type offlineTaskListClient interface {
	ListOfflineTask(page int64) (driver.OfflineTaskResp, error)
}

func loadAllOfflineTasks(listClient offlineTaskListClient) ([]*driver.OfflineTask, int64, error) {
	first, err := listClient.ListOfflineTask(1)
	if err != nil {
		return nil, 0, err
	}
	if first.Page > 0 && first.Page != 1 {
		return nil, 0, fmt.Errorf("offline task list returned page %d for requested page 1", first.Page)
	}
	if first.Total < 0 {
		return nil, 0, fmt.Errorf("offline task list returned negative total %d", first.Total)
	}

	tasks := make([]*driver.OfflineTask, 0, len(first.Tasks))
	seen := make(map[string]struct{}, len(first.Tasks))
	appendUnique := func(items []*driver.OfflineTask) {
		for _, task := range items {
			if task == nil {
				continue
			}
			hash := strings.TrimSpace(task.InfoHash)
			if hash != "" {
				key := strings.ToLower(hash)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
			}
			tasks = append(tasks, task)
		}
	}
	appendUnique(first.Tasks)

	total := first.Total
	if int64(len(tasks)) > total {
		total = int64(len(tasks))
	}
	maxPage := first.PageCount
	if maxPage <= 1 || first.Total == 0 {
		return tasks, total, nil
	}

	// Treat the first page as the snapshot boundary. A live task list may shrink
	// or grow while it is being read; shrinking can end the walk early, but a
	// later page_count increase must never turn the snapshot into an unbounded
	// chase of newly arriving tasks.
	for page := int64(2); page <= maxPage; page++ {
		result, err := listClient.ListOfflineTask(page)
		if err != nil {
			return nil, 0, err
		}
		if result.Page > 0 && result.Page != page {
			return nil, 0, fmt.Errorf("offline task list returned page %d for requested page %d", result.Page, page)
		}
		appendUnique(result.Tasks)
		if int64(len(tasks)) >= first.Total {
			break
		}
		if result.PageCount > 0 && page >= result.PageCount {
			break
		}
	}
	if int64(len(tasks)) > total {
		total = int64(len(tasks))
	}
	return tasks, total, nil
}

type offlineRemovePlanItem struct {
	Hash         string  `json:"hash"`
	Visible      bool    `json:"visible"`
	Name         string  `json:"name,omitempty"`
	Status       string  `json:"status,omitempty"`
	Size         int64   `json:"size,omitempty"`
	Percent      float64 `json:"percent,omitempty"`
	FileID       string  `json:"file_id,omitempty"`
	DeleteFileID string  `json:"delete_file_id,omitempty"`
}

type offlineRemovePlan struct {
	Operation   string                  `json:"operation"`
	DryRun      bool                    `json:"dry_run"`
	DeleteFiles bool                    `json:"delete_files"`
	Items       []offlineRemovePlanItem `json:"items"`
	Note        string                  `json:"note"`
}

func buildOfflineRemovePlan(listClient offlineTaskListClient, hashes []string, deleteFiles bool) (offlineRemovePlan, error) {
	plan := offlineRemovePlan{Operation: "offline-rm", DryRun: true, DeleteFiles: deleteFiles, Note: "hashes not visible in the current task list are still accepted for submission; server-side deletion validation is deferred"}
	if deleteFiles {
		plan.Note = "associated file IDs are shown when visible; hashes not visible in the current task list are still accepted for submission, so their file impact remains server-side until execution"
	}
	tasks, _, err := loadAllOfflineTasks(listClient)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	visible := make(map[string]*driver.OfflineTask, len(tasks))
	for _, task := range tasks {
		if task == nil {
			continue
		}
		visible[strings.ToLower(strings.TrimSpace(task.InfoHash))] = task
	}
	plan.Items = make([]offlineRemovePlanItem, 0, len(hashes))
	for _, hash := range hashes {
		if strings.TrimSpace(hash) == "" {
			return plan, &exitError{code: output.ExitArgs, msg: "offline task hash must not be empty"}
		}
		item := offlineRemovePlanItem{Hash: hash}
		if task := visible[strings.ToLower(strings.TrimSpace(hash))]; task != nil {
			item.Visible = true
			item.Name = task.Name
			item.Status = task.GetStatus()
			item.Size = task.Size
			item.Percent = task.Percent
			item.FileID = task.FileId
			item.DeleteFileID = task.DelFileId
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

func printOfflineAddPlan(plan offlineAddPlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN offline add: %d URL(s), save dir %q (%s); no task was submitted.\n", len(plan.URLs), plan.SaveDirectory.Path, plan.SaveDirectory.ID)
}

func printOfflineRemovePlan(plan offlineRemovePlan) {
	if jsonOutput {
		return
	}
	visible := 0
	for _, item := range plan.Items {
		if item.Visible {
			visible++
		}
	}
	if plan.DeleteFiles {
		fmt.Printf("DRY-RUN offline rm: %d hash(es), %d currently visible; no task or associated file was removed.\n", len(plan.Items), visible)
		return
	}
	fmt.Printf("DRY-RUN offline rm: %d hash(es), %d currently visible; no task was removed.\n", len(plan.Items), visible)
}

type sessionRemovePreview struct {
	ID                        string `json:"id"`
	Directory                 string `json:"directory"`
	State                     string `json:"state"`
	Direction                 string `json:"direction,omitempty"`
	Kind                      string `json:"kind,omitempty"`
	InUse                     bool   `json:"in_use"`
	AbortRemote               bool   `json:"abort_remote"`
	RemoteMultipartCandidates int    `json:"remote_multipart_candidates"`
}

func previewSessionRemove(entry transfer.SessionEntry, abortRemote bool) (sessionRemovePreview, error) {
	preview := sessionRemovePreview{
		ID: entry.ID, Directory: entry.Dir, State: entry.Manifest.State, Direction: entry.Manifest.Identity.Direction,
		Kind: entry.Manifest.Identity.Kind, InUse: entry.InUse, AbortRemote: abortRemote,
	}
	if entry.NewerVersion {
		return preview, sessionExitError(fmt.Errorf("%w: version %d", transfer.ErrSessionNewerVersion, entry.Manifest.Version))
	}
	if entry.InUse {
		return preview, sessionExitError(transfer.ErrSessionLocked)
	}
	if !abortRemote {
		return preview, nil
	}
	if entry.Corrupt {
		return preview, &exitError{code: output.ExitArgs, msg: "cannot abort remote multipart for an unreadable session"}
	}
	if entry.Manifest.Identity.Direction != "upload" {
		return preview, &exitError{code: output.ExitArgs, msg: "--abort-remote is only valid for upload sessions"}
	}
	if client == nil || client.UserID == 0 {
		return preview, &exitError{code: output.ExitArgs, msg: "--abort-remote requires an authenticated 115 account"}
	}
	if entry.Manifest.AccountID == 0 {
		return preview, &exitError{code: output.ExitArgs, msg: "session account is not bound; resume it once before using --abort-remote"}
	}
	if entry.Manifest.AccountID != client.UserID {
		return preview, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("session belongs to account %d, current account is %d", entry.Manifest.AccountID, client.UserID)}
	}
	paths, err := uploadSessionResumePaths(entry)
	if err != nil {
		return preview, &exitError{code: output.ExitError, msg: err.Error()}
	}
	for _, statePath := range paths {
		active, inspectErr := uploadpkg.InspectRemoteResumeMultipart(statePath)
		if inspectErr != nil {
			return preview, &exitError{code: output.ExitError, msg: inspectErr.Error()}
		}
		if active {
			preview.RemoteMultipartCandidates++
		}
	}
	return preview, nil
}

func runSessionsRemoveDryRun(cmd *cobra.Command, store transfer.SessionStore, inputs []string) error {
	if len(inputs) == 1 {
		plans := prepareSessionRemovePlans(store, inputs)
		if plans[0].Err != nil {
			return plans[0].Err
		}
		preview, err := previewSessionRemove(plans[0].Entry, sessionsRmAbortRemote)
		if err != nil {
			return err
		}
		printer.PrintSuccess(preview)
		if !jsonOutput {
			fmt.Printf("DRY-RUN sessions rm %s: trash session", preview.ID)
			if preview.AbortRemote {
				fmt.Printf(", %d remote multipart candidate(s) would be aborted", preview.RemoteMultipartCandidates)
			}
			fmt.Println("; no session state was changed.")
		}
		return nil
	}
	plans := prepareSessionRemovePlans(store, inputs)
	continueOnError := batchContinueOnError(cmd)
	previews := make([]sessionRemovePreview, 0, len(inputs))
	items := make([]batchItemResult, 0, len(inputs))
	for i, plan := range plans {
		if plan.Err != nil {
			items = append(items, failedBatchItem(plan.Input, map[string]interface{}{"dry_run": true, "id": plan.Input}, plan.Err))
			printBatchItemFailure(i, len(inputs), "plan sessions rm "+plan.Input, plan.Err)
			if !continueOnError {
				break
			}
			continue
		}
		preview, err := previewSessionRemove(plan.Entry, sessionsRmAbortRemote)
		if err != nil {
			items = append(items, failedBatchItem(plan.Input, preview, err))
			printBatchItemFailure(i, len(inputs), "plan sessions rm "+plan.Input, err)
			if !continueOnError {
				break
			}
			continue
		}
		previews = append(previews, preview)
		items = append(items, successfulBatchItem(plan.Input, preview))
		if !jsonOutput {
			fmt.Printf("DRY-RUN sessions rm %s: trash session", preview.ID)
			if preview.AbortRemote {
				fmt.Printf(", %d remote multipart candidate(s) would be aborted", preview.RemoteMultipartCandidates)
			}
			fmt.Println("; no session state was changed.")
		}
	}
	data := batchResultData(len(inputs), items, map[string]interface{}{"dry_run": true, "operation": "sessions-rm", "plans": previews})
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("sessions rm dry-run", len(inputs), items, data)
	}
	printer.PrintSuccess(data)
	return nil
}

func uploadSessionResumePaths(entry transfer.SessionEntry) ([]string, error) {
	if entry.Corrupt || entry.NewerVersion {
		return nil, errors.New("cannot inspect multipart state for an unreadable session")
	}
	switch entry.Manifest.Identity.Kind {
	case "file":
		return []string{filepath.Join(entry.Dir, "payload.json")}, nil
	case "tree":
		partsDir := filepath.Join(entry.Dir, "parts")
		parts, err := os.ReadDir(partsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("read upload session parts: %w", err)
		}
		paths := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.IsDir() || !strings.HasSuffix(part.Name(), ".upload.json") {
				continue
			}
			paths = append(paths, filepath.Join(partsDir, part.Name()))
		}
		return paths, nil
	default:
		return nil, fmt.Errorf("unsupported upload session kind %q", entry.Manifest.Identity.Kind)
	}
}
