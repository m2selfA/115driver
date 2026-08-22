package tools

import (
	"context"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPRecycleBatchPages   = 256
	maxMCPRecycleBatchEntries = 5000
)

// ListRecyclePagesItem defines one independently paginated recycle-bin request.
type ListRecyclePagesItem struct {
	Offset int `json:"offset,omitempty" jsonschema:"zero-based recycle-bin offset, default 0"`
	Limit  int `json:"limit,omitempty" jsonschema:"page size, default 40, maximum 100"`
}

// ListRecyclePagesArgs defines a bounded recycle-bin listing batch.
type ListRecyclePagesArgs struct {
	Pages []ListRecyclePagesItem `json:"pages" jsonschema:"independently paginated recycle-bin requests; exact logical duplicates are rejected"`
}

// ListRecyclePagesItemResult reports one page while preserving input order.
type ListRecyclePagesItemResult struct {
	Index      int                 `json:"index" jsonschema:"zero-based input page index"`
	Offset     int                 `json:"offset" jsonschema:"requested offset"`
	Limit      int                 `json:"limit" jsonschema:"effective page size"`
	Returned   int                 `json:"returned" jsonschema:"number of returned recycle-bin entries"`
	NextOffset *int                `json:"next_offset,omitempty" jsonschema:"conservative continuation offset when this page fills its limit"`
	Success    bool                `json:"success" jsonschema:"whether this page was listed successfully"`
	Error      string              `json:"error,omitempty" jsonschema:"item error when listing failed"`
	Items      []MCPRecycleBinItem `json:"items,omitempty" jsonschema:"recycle-bin entries in server order"`
}

// ListRecyclePagesResult summarizes a bounded recycle listing batch.
type ListRecyclePagesResult struct {
	Requested int                          `json:"requested" jsonschema:"number of requested recycle pages"`
	Succeeded int                          `json:"succeeded" jsonschema:"number of successful pages"`
	Failed    int                          `json:"failed" jsonschema:"number of failed pages"`
	Items     []ListRecyclePagesItemResult `json:"items" jsonschema:"per-page results in input order"`
}

type mcpPreparedRecyclePage struct {
	offset int
	limit  int
}

func prepareMCPRecyclePageBatch(args ListRecyclePagesArgs) ([]mcpPreparedRecyclePage, error) {
	if len(args.Pages) == 0 {
		return nil, fmt.Errorf("at least one recycle page is required")
	}
	if len(args.Pages) > maxMCPRecycleBatchPages {
		return nil, fmt.Errorf("received %d recycle pages; maximum is %d", len(args.Pages), maxMCPRecycleBatchPages)
	}
	prepared := make([]mcpPreparedRecyclePage, len(args.Pages))
	seen := make(map[[2]int]int, len(args.Pages))
	budget := 0
	for i, page := range args.Pages {
		offset, limit, err := validateRecyclePagination(page.Offset, page.Limit)
		if err != nil {
			return nil, fmt.Errorf("recycle page %d pagination: %w", i, err)
		}
		budget += limit
		if budget > maxMCPRecycleBatchEntries {
			return nil, fmt.Errorf("recycle batch effective page budget %d exceeds maximum %d", budget, maxMCPRecycleBatchEntries)
		}
		key := [2]int{offset, limit}
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("recycle pages %d and %d are exact logical duplicates", previous, i)
		}
		seen[key] = i
		prepared[i] = mcpPreparedRecyclePage{offset: offset, limit: limit}
	}
	return prepared, nil
}

func mcpRecycleBinItems(items []driver.RecycleBinItem) []MCPRecycleBinItem {
	typed := make([]MCPRecycleBinItem, len(items))
	for i, item := range items {
		typed[i] = MCPRecycleBinItem{
			ID: item.FileId, FileName: item.FileName, FileSize: int64(item.FileSize),
			ParentID: string(item.ParentId), ParentName: item.ParentName, DeleteTime: int64(item.DeleteTime),
		}
	}
	return typed
}

func listRecyclePagesCallResult(response ListRecyclePagesResult) (*mcp.CallToolResult, ListRecyclePagesResult, error) {
	return mcpTypedJSONResult("list_recycle_pages", response, response, response.Failed > 0)
}

func (rt *RecycleTools) listRecyclePages(ctx context.Context, req *mcp.CallToolRequest, args ListRecyclePagesArgs) (*mcp.CallToolResult, ListRecyclePagesResult, error) {
	prepared, err := prepareMCPRecyclePageBatch(args)
	if err != nil {
		return toolError(fmt.Sprintf("list_recycle_pages preflight failed: %v", err)), ListRecyclePagesResult{}, nil
	}
	if rt.client == nil {
		return toolError("115 client is unavailable"), ListRecyclePagesResult{}, nil
	}

	response := ListRecyclePagesResult{Requested: len(prepared), Items: make([]ListRecyclePagesItemResult, len(prepared))}
	for i, page := range prepared {
		entry := ListRecyclePagesItemResult{Index: i, Offset: page.offset, Limit: page.limit}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		items, err := rt.client.ListRecycleBin(page.offset, page.limit)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Items = mcpRecycleBinItems(items)
		entry.Returned = len(entry.Items)
		if entry.Returned == page.limit {
			next := page.offset + entry.Returned
			entry.NextOffset = &next
		}
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return listRecyclePagesCallResult(response)
}
