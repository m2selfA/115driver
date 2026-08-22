package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

const (
	defaultRecycleListLimit = 40
	maxRecycleListLimit     = 100
)

var (
	recycleOffset        int
	recycleLimit         = defaultRecycleListLimit
	recycleRestoreDryRun bool
)

type recycleListClient interface {
	ListRecycleBin(offset, limit int) ([]driver.RecycleBinItem, error)
}

type recycleRestoreClient interface {
	recycleListClient
	RevertRecycleBin(rIDs ...string) error
}

type recycleListItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ParentID   string `json:"parent_id,omitempty"`
	ParentName string `json:"parent_name,omitempty"`
	DeletedAt  int64  `json:"deleted_at"`
}

type recycleListResult struct {
	Offset     int               `json:"offset"`
	Limit      int               `json:"limit"`
	Returned   int               `json:"returned"`
	PageFull   bool              `json:"page_full"`
	NextOffset int               `json:"next_offset"`
	Items      []recycleListItem `json:"items"`
}

var recycleCmd = &cobra.Command{
	Use:   "recycle",
	Short: "Inspect and restore recycle-bin items",
	Args:  cobra.NoArgs,
}

var recycleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recycle-bin items",
	Args:  recycleListArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := loadRecycleList(client, recycleOffset, recycleLimit)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("List recycle bin failed: %v", err)}
		}
		printer.PrintSuccess(result)
		printRecycleList(result)
		return nil
	},
}

type recycleRestoreResult struct {
	DryRun      bool              `json:"dry_run"`
	InputCount  int               `json:"input_count"`
	UniqueCount int               `json:"unique_count"`
	RestoredIDs []string          `json:"restored_ids,omitempty"`
	Items       []recycleListItem `json:"items,omitempty"`
}

var recycleRestoreCmd = &cobra.Command{
	Use:     "restore <item_id>...",
	Aliases: []string{"revert"},
	Short:   "Restore one or more recycle-bin items",
	Long:    "Restore one or more recycle-bin items in a single 115 batch request. --from-file FILE reads additional item IDs one per line. --dry-run verifies that every requested ID is currently visible in the recycle bin without changing remote data.",
	Args:    batchInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRecycleRestore(client, cmd, args)
	},
}

func runRecycleRestore(restoreClient recycleRestoreClient, cmd *cobra.Command, args []string) error {
	expanded, err := expandBatchInputArgs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	ids, err := normalizeRecycleRestoreIDs(expanded)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	result := recycleRestoreResult{DryRun: recycleRestoreDryRun, InputCount: len(expanded), UniqueCount: len(ids)}
	if recycleRestoreDryRun {
		items, missing, err := findRecycleItems(restoreClient, ids)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Inspect recycle bin failed: %v", err)}
		}
		result.Items = items
		if len(missing) > 0 {
			return &exitError{code: output.ExitNotFound, msg: fmt.Sprintf("recycle item(s) not found: %s", strings.Join(missing, ", ")), data: result}
		}
		printer.PrintSuccess(result)
		if !jsonOutput {
			fmt.Printf("DRY-RUN recycle restore: %d item(s) validated; no remote data changed.\n", len(ids))
		}
		return nil
	}
	if err := restoreClient.RevertRecycleBin(ids...); err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Restore recycle item(s) failed: %v", err)}
	}
	result.RestoredIDs = append([]string(nil), ids...)
	printer.PrintSuccess(result)
	if !jsonOutput {
		fmt.Printf("Restored %d recycle-bin item(s).\n", len(ids))
	}
	return nil
}

func normalizeRecycleRestoreIDs(inputs []string) ([]string, error) {
	ids := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, raw := range inputs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("recycle item ID must not be empty")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("at least one recycle item ID is required")
	}
	return ids, nil
}

func recyclePageSignature(page []driver.RecycleBinItem) string {
	var b strings.Builder
	for i := range page {
		b.WriteString(strings.TrimSpace(page[i].FileId))
		b.WriteByte(0)
	}
	return b.String()
}

func findRecycleItems(listClient recycleListClient, ids []string) ([]recycleListItem, []string, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	found := make(map[string]recycleListItem, len(ids))
	previousFullPage := ""
	for offset := 0; len(found) < len(wanted); {
		page, err := listClient.ListRecycleBin(offset, maxRecycleListLimit)
		if err != nil {
			return nil, nil, err
		}
		if len(page) == maxRecycleListLimit {
			signature := recyclePageSignature(page)
			if previousFullPage != "" && signature == previousFullPage {
				return nil, nil, fmt.Errorf("recycle pagination repeated a full page at offset %d: %w", offset, driver.ErrUnexpected)
			}
			previousFullPage = signature
		} else {
			previousFullPage = ""
		}
		for _, item := range page {
			if _, ok := wanted[item.FileId]; !ok {
				continue
			}
			found[item.FileId] = recycleListItem{
				ID: item.FileId, Name: item.FileName, Size: int64(item.FileSize), ParentID: string(item.ParentId),
				ParentName: item.ParentName, DeletedAt: int64(item.DeleteTime),
			}
		}
		if len(page) < maxRecycleListLimit {
			break
		}
		offset += len(page)
	}
	items := make([]recycleListItem, 0, len(found))
	missing := make([]string, 0)
	for _, id := range ids {
		if item, ok := found[id]; ok {
			items = append(items, item)
		} else {
			missing = append(missing, id)
		}
	}
	return items, missing, nil
}

func recycleListArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if recycleOffset < 0 {
		return &exitError{code: output.ExitArgs, msg: "--offset must be >= 0"}
	}
	if recycleLimit <= 0 || recycleLimit > maxRecycleListLimit {
		return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("--limit must be between 1 and %d", maxRecycleListLimit)}
	}
	return nil
}

func loadRecycleList(listClient recycleListClient, offset, limit int) (recycleListResult, error) {
	result := recycleListResult{Offset: offset, Limit: limit}
	items, err := listClient.ListRecycleBin(offset, limit)
	if err != nil {
		return result, err
	}
	result.Items = make([]recycleListItem, 0, len(items))
	for _, item := range items {
		result.Items = append(result.Items, recycleListItem{
			ID: item.FileId, Name: item.FileName, Size: int64(item.FileSize), ParentID: string(item.ParentId),
			ParentName: item.ParentName, DeletedAt: int64(item.DeleteTime),
		})
	}
	result.Returned = len(result.Items)
	result.PageFull = result.Returned == limit
	result.NextOffset = offset + result.Returned
	return result, nil
}

func printRecycleList(result recycleListResult) {
	if jsonOutput {
		return
	}
	fmt.Printf("Recycle bin: %d item(s) returned\n\n", result.Returned)
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "SIZE\tDELETED\tID\tNAME\tPARENT")
	for _, item := range result.Items {
		fmt.Fprintf(writer, "%s\t%d\t%s\t%s\t%s\n", output.FormatFileSize(item.Size), item.DeletedAt, item.ID, item.Name, item.ParentName)
	}
	_ = writer.Flush()
	if result.PageFull {
		fmt.Printf("\nPage is full. Use --offset %d to request the next page.\n", result.NextOffset)
	}
}

func init() {
	recycleListCmd.Flags().IntVar(&recycleOffset, "offset", 0, "Offset for paginated recycle-bin results")
	recycleListCmd.Flags().IntVar(&recycleLimit, "limit", defaultRecycleListLimit, "Max recycle-bin entries to return (1-100)")
	recycleRestoreCmd.Flags().BoolVar(&recycleRestoreDryRun, "dry-run", false, "Validate requested recycle IDs without restoring anything")
	addBatchFromFileFlag(recycleRestoreCmd)
	recycleCmd.AddCommand(recycleListCmd, recycleRestoreCmd)
	rootCmd.AddCommand(recycleCmd)
}
