package tools

import (
	"context"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPOfflineBatchPages = 128
	maxMCPOfflineBatchTasks = 5000
)

// ListOfflinePagesArgs defines a bounded set of offline-task pages. Page 0 is
// normalized to page 1 for compatibility with listOfflineTasks.
type ListOfflinePagesArgs struct {
	Pages []int64 `json:"pages" jsonschema:"offline-task page numbers; 0 means page 1 and duplicate logical pages are rejected"`
}

// ListOfflinePagesItemResult reports one page while preserving input order.
type ListOfflinePagesItemResult struct {
	Index    int                       `json:"index" jsonschema:"zero-based input page index"`
	Page     int64                     `json:"page" jsonschema:"normalized requested page number"`
	NextPage *int64                    `json:"next_page,omitempty" jsonschema:"exact next page when server pagination metadata proves more pages exist"`
	Success  bool                      `json:"success" jsonschema:"whether this page was listed successfully"`
	Error    string                    `json:"error,omitempty" jsonschema:"item error when listing failed"`
	Data     *MCPOfflineTaskListOutput `json:"data,omitempty" jsonschema:"credential-free offline-task page when successful"`
}

// ListOfflinePagesResult summarizes a bounded multi-page offline listing.
type ListOfflinePagesResult struct {
	Requested       int                          `json:"requested" jsonschema:"number of requested pages"`
	Succeeded       int                          `json:"succeeded" jsonschema:"number of successful pages"`
	Failed          int                          `json:"failed" jsonschema:"number of failed or budget-skipped pages"`
	TasksReturned   int                          `json:"tasks_returned" jsonschema:"aggregate task records returned across successful pages"`
	MaxTasks        int                          `json:"max_tasks" jsonschema:"aggregate task output budget"`
	BudgetExhausted bool                         `json:"budget_exhausted" jsonschema:"whether the aggregate task budget prevented returning a requested page"`
	Items           []ListOfflinePagesItemResult `json:"items" jsonschema:"per-page results in input order"`
}

func prepareMCPOfflinePageBatch(args ListOfflinePagesArgs) ([]int64, error) {
	if len(args.Pages) == 0 {
		return nil, fmt.Errorf("at least one offline page is required")
	}
	if len(args.Pages) > maxMCPOfflineBatchPages {
		return nil, fmt.Errorf("received %d offline pages; maximum is %d", len(args.Pages), maxMCPOfflineBatchPages)
	}
	pages := make([]int64, len(args.Pages))
	seen := make(map[int64]int, len(args.Pages))
	for i, raw := range args.Pages {
		page, err := normalizeOfflinePage(raw)
		if err != nil {
			return nil, fmt.Errorf("offline page %d: %w", i, err)
		}
		if previous, ok := seen[page]; ok {
			return nil, fmt.Errorf("offline pages %d and %d resolve to the same page %d", previous, i, page)
		}
		seen[page] = i
		pages[i] = page
	}
	return pages, nil
}

func mcpOfflineTaskListOutput(result driver.OfflineTaskResp) MCPOfflineTaskListOutput {
	tasks := make([]MCPOfflineTask, len(result.Tasks))
	for i, task := range result.Tasks {
		if task == nil {
			continue
		}
		tasks[i] = MCPOfflineTask{
			InfoHash: task.InfoHash, Name: task.Name, Size: task.Size, AddTime: task.AddTime,
			Peers: task.Peers, RateDownload: task.RateDownload, Status: task.Status,
			StatusText: task.GetStatus(), Percent: task.Percent, UpdateTime: task.UpdateTime,
			LeftTime: task.LeftTime, FileID: task.FileId, DeleteFileID: task.DelFileId,
			DirID: task.DirId, Move: task.Move,
		}
	}
	return MCPOfflineTaskListOutput{
		Total: result.Total, Count: result.Count, PageRow: result.PageRow,
		PageCount: result.PageCount, Page: result.Page, Quota: result.Quota, Tasks: tasks,
	}
}

func listOfflinePagesCallResult(response ListOfflinePagesResult) (*mcp.CallToolResult, ListOfflinePagesResult, error) {
	return mcpTypedJSONResult("list_offline_pages", response, response, response.Failed > 0)
}

func (ot *OfflineTools) listOfflinePages(ctx context.Context, req *mcp.CallToolRequest, args ListOfflinePagesArgs) (*mcp.CallToolResult, ListOfflinePagesResult, error) {
	pages, err := prepareMCPOfflinePageBatch(args)
	if err != nil {
		return toolError(fmt.Sprintf("list_offline_pages preflight failed: %v", err)), ListOfflinePagesResult{}, nil
	}
	if ot.client == nil {
		return toolError("115 client is unavailable"), ListOfflinePagesResult{}, nil
	}

	response := ListOfflinePagesResult{
		Requested: len(pages), MaxTasks: maxMCPOfflineBatchTasks,
		Items: make([]ListOfflinePagesItemResult, len(pages)),
	}
	for i, page := range pages {
		entry := ListOfflinePagesItemResult{Index: i, Page: page}
		if response.BudgetExhausted {
			entry.Error = "offline task output budget exhausted before this page could be requested"
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		result, err := ot.client.ListOfflineTask(page)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if response.TasksReturned+len(result.Tasks) > maxMCPOfflineBatchTasks {
			response.BudgetExhausted = true
			entry.Error = fmt.Sprintf("offline task output budget would exceed %d tasks", maxMCPOfflineBatchTasks)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		data := mcpOfflineTaskListOutput(result)
		entry.Data = &data
		if result.Page > 0 && result.PageCount > 0 && result.Page < result.PageCount {
			nextPage := result.Page + 1
			entry.NextPage = &nextPage
		}
		entry.Success = true
		response.TasksReturned += len(data.Tasks)
		response.Succeeded++
		response.Items[i] = entry
		if response.TasksReturned == maxMCPOfflineBatchTasks && i+1 < len(pages) {
			response.BudgetExhausted = true
		}
	}
	return listOfflinePagesCallResult(response)
}
