package cmd

import (
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

var (
	offlineSaveDir       string
	offlineAddDryRun     bool
	offlineRmDryRun      bool
	offlineRmDeleteFiles bool
	offlineRmForce       bool
	offlineClearScope    = "completed"
	offlineClearDryRun   bool
	offlineClearForce    bool
)

var offlineCmd = &cobra.Command{
	Use:   "offline",
	Short: "Manage offline downloads",
	Args:  cobra.NoArgs,
}

var offlineAddCmd = &cobra.Command{
	Use:   "add <url>...",
	Short: "Add one or more offline download tasks",
	Args:  batchInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		if offlineAddDryRun {
			plan, err := buildOfflineAddPlan(client, args)
			if err != nil {
				return err
			}
			printer.PrintSuccess(plan)
			printOfflineAddPlan(plan)
			return nil
		}
		saveDir, err := resolveOfflineSaveDirectory(client)
		if err != nil {
			return err
		}
		saveDirID, saveDirName := saveDir.ID, saveDir.Path

		if len(args) > 1 && batchContinueOnError(cmd) {
			return runOfflineAddContinueOnError(args, saveDirID, saveDirName)
		}
		hashes, err := client.AddOfflineTaskURIs(args, saveDirID)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}

		data := map[string]interface{}{
			"urls":     append([]string(nil), args...),
			"hashes":   hashes,
			"save_dir": saveDirName,
		}
		if len(args) == 1 {
			data["url"] = args[0]
		} else {
			items := make([]batchItemResult, 0, len(args))
			for i, url := range args {
				itemData := map[string]interface{}{"url": url}
				if len(hashes) == len(args) {
					itemData["hash"] = hashes[i]
				}
				items = append(items, successfulBatchItem(url, itemData))
			}
			data = batchResultData(len(args), items, data)
		}
		printer.PrintSuccess(data)
		if !jsonOutput {
			if len(args) == 1 {
				fmt.Printf("Offline task added: %s\n", args[0])
			} else {
				fmt.Printf("Offline tasks added: %d URL(s)\n", len(args))
			}
		}
		return nil
	},
}

var offlineListCmd = &cobra.Command{
	Use:   "list",
	Short: "List offline download tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		allTasks, total, err := loadAllOfflineTasks(client)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}

		tasks := make([]map[string]interface{}, 0, len(allTasks))
		for _, t := range allTasks {
			tasks = append(tasks, map[string]interface{}{
				"name":    t.Name,
				"hash":    t.InfoHash,
				"status":  t.GetStatus(),
				"percent": t.Percent,
				"size":    t.Size,
			})
		}

		if jsonOutput {
			printer.PrintSuccess(map[string]interface{}{
				"total": total,
				"tasks": tasks,
			})
		} else {
			fmt.Printf("Offline tasks (%d total):\n\n", total)
			printer.PrintOfflineTable(tasks)
		}
		return nil
	},
}

type offlineRemoveClient interface {
	offlineTaskListClient
	DeleteOfflineTasks(hashes []string, deleteFiles bool) error
}

var offlineRmCmd = &cobra.Command{
	Use:   "rm <hash>...",
	Short: "Remove one or more offline download tasks",
	Long:  "Remove one or more offline task records. By default associated downloaded files are preserved. --delete-files also deletes associated files and therefore requires --force for actual execution; --dry-run previews visible task/file metadata without removing anything.",
	Args:  offlineRmArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOfflineRemove(client, cmd, args)
	},
}

func offlineRmArgs(cmd *cobra.Command, args []string) error {
	if err := batchInputArgs(cmd, args); err != nil {
		return err
	}
	if offlineRmDeleteFiles && !offlineRmDryRun && !offlineRmForce {
		return &exitError{code: output.ExitArgs, msg: "offline rm --delete-files requires --force; use --dry-run to review associated file IDs first"}
	}
	return nil
}

func runOfflineRemove(removeClient offlineRemoveClient, cmd *cobra.Command, args []string) error {
	expandedArgs, err := expandBatchInputArgs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	args = expandedArgs
	if offlineRmDryRun {
		plan, err := buildOfflineRemovePlan(removeClient, args, offlineRmDeleteFiles)
		if err != nil {
			return err
		}
		printer.PrintSuccess(plan)
		printOfflineRemovePlan(plan)
		return nil
	}
	if len(args) > 1 && batchContinueOnError(cmd) {
		return runOfflineRmContinueOnError(removeClient, args, offlineRmDeleteFiles)
	}
	if err := removeClient.DeleteOfflineTasks(args, offlineRmDeleteFiles); err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}

	data := map[string]interface{}{"deleted_hashes": append([]string(nil), args...), "delete_files": offlineRmDeleteFiles}
	if len(args) > 1 {
		items := make([]batchItemResult, 0, len(args))
		for _, hash := range args {
			items = append(items, successfulBatchItem(hash, map[string]interface{}{"hash": hash, "delete_files": offlineRmDeleteFiles}))
		}
		data = batchResultData(len(args), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		suffix := ""
		if offlineRmDeleteFiles {
			suffix = " and associated files"
		}
		if len(args) == 1 {
			fmt.Printf("Removed offline task%s: %s\n", suffix, args[0])
		} else {
			fmt.Printf("Removed offline tasks%s: %d\n", suffix, len(args))
		}
	}
	return nil
}

type offlineClearClient interface {
	offlineTaskListClient
	ClearOfflineTasks(clearFlag int64) error
}

type offlineClearPlanItem struct {
	Hash    string  `json:"hash"`
	Name    string  `json:"name,omitempty"`
	Status  string  `json:"status"`
	Percent float64 `json:"percent"`
	Size    int64   `json:"size"`
}

type offlineClearPlan struct {
	Operation string                 `json:"operation"`
	Scope     string                 `json:"scope"`
	DryRun    bool                   `json:"dry_run"`
	Total     int64                  `json:"total"`
	Matched   int                    `json:"matched"`
	Items     []offlineClearPlanItem `json:"items"`
}

var offlineClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear selected offline task records",
	Long:  "Clear completed, failed, active, or all offline task records. The default scope is completed. Active covers queued/not-yet-started and currently downloading tasks. --dry-run lists the currently matching tasks without clearing anything. Actual clearing always requires --force because this is a broad remote mutation. This command never uses the 115 clear modes that also delete downloaded source files.",
	Args:  offlineClearArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOfflineClear(client)
	},
}

func offlineClearArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if _, _, err := resolveOfflineClearScope(offlineClearScope); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if !offlineClearDryRun && !offlineClearForce {
		return &exitError{code: output.ExitArgs, msg: "offline clear requires --force; use --dry-run to review the current matching tasks"}
	}
	return nil
}

func resolveOfflineClearScope(raw string) (string, int64, error) {
	scope := strings.ToLower(strings.TrimSpace(raw))
	switch scope {
	case "completed":
		return scope, 0, nil
	case "all":
		return scope, 1, nil
	case "failed":
		return scope, 2, nil
	case "active":
		return scope, 3, nil
	default:
		return "", 0, fmt.Errorf("unsupported offline clear scope %q; use \"completed\", \"failed\", \"active\", or \"all\"", raw)
	}
}

func buildOfflineClearPlan(listClient offlineTaskListClient, scope string) (offlineClearPlan, error) {
	plan := offlineClearPlan{Operation: "offline-clear", Scope: scope, DryRun: true, Items: []offlineClearPlanItem{}}
	tasks, total, err := loadAllOfflineTasks(listClient)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	plan.Total = total
	for _, task := range tasks {
		if task == nil {
			continue
		}
		matches := true
		switch scope {
		case "completed":
			matches = task.IsDone()
		case "failed":
			matches = task.IsFailed()
		case "active":
			matches = task.IsTodo() || task.IsRunning()
		}
		if !matches {
			continue
		}
		plan.Items = append(plan.Items, offlineClearPlanItem{
			Hash: task.InfoHash, Name: task.Name, Status: task.GetStatus(), Percent: task.Percent, Size: task.Size,
		})
	}
	plan.Matched = len(plan.Items)
	return plan, nil
}

func runOfflineClear(clearClient offlineClearClient) error {
	scope, flag, err := resolveOfflineClearScope(offlineClearScope)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	plan, err := buildOfflineClearPlan(clearClient, scope)
	if err != nil {
		return err
	}
	if offlineClearDryRun {
		printer.PrintSuccess(plan)
		if !jsonOutput {
			fmt.Printf("DRY-RUN offline clear: scope=%s, %d of %d task(s) currently match; nothing was cleared.\n", scope, plan.Matched, plan.Total)
		}
		return nil
	}
	if plan.Matched == 0 {
		printer.PrintSuccess(map[string]interface{}{
			"operation": "offline-clear", "scope": scope, "cleared": false,
			"observed_matching_before_clear": 0, "total_before_clear": plan.Total,
		})
		if !jsonOutput {
			fmt.Printf("No offline tasks currently match scope %s; nothing was cleared.\n", scope)
		}
		return nil
	}
	if err := clearClient.ClearOfflineTasks(flag); err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Clear offline tasks failed: %v", err)}
	}
	printer.PrintSuccess(map[string]interface{}{
		"operation": "offline-clear", "scope": scope, "cleared": true,
		"observed_matching_before_clear": plan.Matched, "total_before_clear": plan.Total,
	})
	if !jsonOutput {
		fmt.Printf("Offline tasks cleared: scope=%s (%d matching before request).\n", scope, plan.Matched)
	}
	return nil
}

func runOfflineAddContinueOnError(urls []string, saveDirID, saveDirName string) error {
	items := make([]batchItemResult, 0, len(urls))
	allHashes := make([]string, 0, len(urls))
	for i, url := range urls {
		hashes, err := client.AddOfflineTaskURIs([]string{url}, saveDirID)
		itemData := map[string]interface{}{"url": url}
		if err != nil {
			items = append(items, failedBatchItem(url, itemData, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}))
			printBatchItemFailure(i, len(urls), "offline add "+url, err)
			continue
		}
		itemData["hashes"] = hashes
		allHashes = append(allHashes, hashes...)
		items = append(items, successfulBatchItem(url, itemData))
		if !jsonOutput {
			fmt.Printf("Offline task added: %s\n", url)
		}
	}
	data := batchResultData(len(urls), items, map[string]interface{}{
		"urls":     append([]string(nil), urls...),
		"hashes":   allHashes,
		"save_dir": saveDirName,
	})
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("offline add batch", len(urls), items, data)
	}
	printer.PrintSuccess(data)
	return nil
}

func runOfflineRmContinueOnError(removeClient offlineRemoveClient, hashes []string, deleteFiles bool) error {
	items := make([]batchItemResult, 0, len(hashes))
	deleted := make([]string, 0, len(hashes))
	for i, hash := range hashes {
		err := removeClient.DeleteOfflineTasks([]string{hash}, deleteFiles)
		itemData := map[string]interface{}{"hash": hash, "delete_files": deleteFiles}
		if err != nil {
			items = append(items, failedBatchItem(hash, itemData, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}))
			printBatchItemFailure(i, len(hashes), "offline rm "+hash, err)
			continue
		}
		deleted = append(deleted, hash)
		items = append(items, successfulBatchItem(hash, itemData))
		if !jsonOutput {
			fmt.Printf("Removed offline task: %s\n", hash)
		}
	}
	data := batchResultData(len(hashes), items, map[string]interface{}{"deleted_hashes": deleted, "delete_files": deleteFiles})
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("offline rm batch", len(hashes), items, data)
	}
	printer.PrintSuccess(data)
	return nil
}

func init() {
	offlineAddCmd.Flags().StringVarP(&offlineSaveDir, "dir", "d", "", "Remote directory to save downloaded files")
	offlineAddCmd.Flags().BoolVar(&offlineAddDryRun, "dry-run", false, "Plan offline task submission without adding tasks")
	addContinueOnErrorFlag(offlineAddCmd)
	addBatchFromFileFlag(offlineAddCmd)
	addContinueOnErrorFlag(offlineRmCmd)
	addBatchFromFileFlag(offlineRmCmd)
	offlineRmCmd.Flags().BoolVar(&offlineRmDryRun, "dry-run", false, "Inspect requested hashes without removing offline tasks or associated files")
	offlineRmCmd.Flags().BoolVar(&offlineRmDeleteFiles, "delete-files", false, "Also delete files associated with the removed offline tasks")
	offlineRmCmd.Flags().BoolVarP(&offlineRmForce, "force", "f", false, "Required with --delete-files for actual execution")
	offlineClearCmd.Flags().StringVar(&offlineClearScope, "scope", "completed", "Tasks to clear: completed, failed, active, or all")
	offlineClearCmd.Flags().BoolVar(&offlineClearDryRun, "dry-run", false, "List currently matching tasks without clearing them")
	offlineClearCmd.Flags().BoolVarP(&offlineClearForce, "force", "f", false, "Required for actual clearing")
	offlineCmd.AddCommand(offlineAddCmd)
	offlineCmd.AddCommand(offlineListCmd)
	offlineCmd.AddCommand(offlineRmCmd)
	offlineCmd.AddCommand(offlineClearCmd)
	rootCmd.AddCommand(offlineCmd)
}
