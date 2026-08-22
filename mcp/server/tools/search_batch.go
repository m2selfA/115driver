package tools

import (
	"context"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPSearchBatchItems   = 256
	defaultMCPSearchLimit    = 30
	maxMCPSearchBatchLimit   = 500
	maxMCPSearchBatchEntries = 5000
)

// SearchManyItem defines one independently paginated search request.
type SearchManyItem struct {
	SearchValue string `json:"search_value" jsonschema:"search keyword; empty preserves the single-search behavior"`
	Offset      int    `json:"offset,omitempty" jsonschema:"zero-based result offset, default 0"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size, default 30, maximum 500"`
	Type        int    `json:"type,omitempty" jsonschema:"file type filter, 0:all 1:folder 2:document 3:image 4:video 5:audio 6:archive"`
	Order       string `json:"order,omitempty" jsonschema:"sort field; empty uses file_name"`
	Asc         int    `json:"asc,omitempty" jsonschema:"sort direction, 0:descending 1:ascending"`
}

// SearchManyArgs defines a bounded read-only multi-search request.
type SearchManyArgs struct {
	Queries []SearchManyItem `json:"queries" jsonschema:"independently paginated search requests; exact logical duplicates are rejected"`
}

// SearchManyItemResult reports one query while preserving input order.
type SearchManyItemResult struct {
	Index      int              `json:"index" jsonschema:"zero-based input query index"`
	Offset     int              `json:"offset" jsonschema:"requested result offset"`
	Limit      int              `json:"limit" jsonschema:"effective page size"`
	Returned   int              `json:"returned" jsonschema:"number of returned matches"`
	NextOffset *int             `json:"next_offset,omitempty" jsonschema:"continuation offset when another page may exist"`
	Success    bool             `json:"success" jsonschema:"whether this search completed successfully"`
	Error      string           `json:"error,omitempty" jsonschema:"item error when search failed"`
	Data       *MCPSearchResult `json:"data,omitempty" jsonschema:"search result when successful"`
}

// SearchManyResult summarizes a bounded multi-search call.
type SearchManyResult struct {
	Requested int                    `json:"requested" jsonschema:"number of requested searches"`
	Succeeded int                    `json:"succeeded" jsonschema:"number of successful searches"`
	Failed    int                    `json:"failed" jsonschema:"number of failed searches"`
	Items     []SearchManyItemResult `json:"items" jsonschema:"per-search results in input order"`
}

type mcpPreparedSearch struct {
	args  SearchManyItem
	limit int
}

type mcpSearchBatchKey struct {
	SearchValue string
	Offset      int
	Limit       int
	Type        int
	Order       string
	Asc         int
}

func prepareMCPSearchBatch(args SearchManyArgs) ([]mcpPreparedSearch, error) {
	if len(args.Queries) == 0 {
		return nil, fmt.Errorf("at least one search query is required")
	}
	if len(args.Queries) > maxMCPSearchBatchItems {
		return nil, fmt.Errorf("received %d search queries; maximum is %d", len(args.Queries), maxMCPSearchBatchItems)
	}

	prepared := make([]mcpPreparedSearch, len(args.Queries))
	seen := make(map[mcpSearchBatchKey]int, len(args.Queries))
	entryBudget := 0
	for i, item := range args.Queries {
		if item.Offset < 0 {
			return nil, fmt.Errorf("search query %d offset must not be negative", i)
		}
		if item.Limit < 0 {
			return nil, fmt.Errorf("search query %d limit must not be negative", i)
		}
		limit := item.Limit
		if limit == 0 {
			limit = defaultMCPSearchLimit
		}
		if limit > maxMCPSearchBatchLimit {
			return nil, fmt.Errorf("search query %d limit %d exceeds maximum %d", i, limit, maxMCPSearchBatchLimit)
		}
		if item.Type < 0 || item.Type > 6 {
			return nil, fmt.Errorf("search query %d type must be between 0 and 6", i)
		}
		if item.Asc != 0 && item.Asc != 1 {
			return nil, fmt.Errorf("search query %d asc must be 0 or 1", i)
		}
		entryBudget += limit
		if entryBudget > maxMCPSearchBatchEntries {
			return nil, fmt.Errorf("search batch effective page budget %d exceeds maximum %d", entryBudget, maxMCPSearchBatchEntries)
		}

		effectiveOrder := item.Order
		if effectiveOrder == "" {
			effectiveOrder = "file_name"
		}
		key := mcpSearchBatchKey{
			SearchValue: item.SearchValue,
			Offset:      item.Offset,
			Limit:       limit,
			Type:        item.Type,
			Order:       effectiveOrder,
			Asc:         item.Asc,
		}
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("search queries %d and %d are exact logical duplicates", previous, i)
		}
		seen[key] = i
		prepared[i] = mcpPreparedSearch{args: item, limit: limit}
	}
	return prepared, nil
}

func mcpSearchResultFromDriver(result *driver.SearchResult) MCPSearchResult {
	if result == nil {
		return MCPSearchResult{}
	}
	files := make([]MCPDirectoryEntry, len(result.Files))
	for i, file := range result.Files {
		files[i] = mcpDirectoryEntryFromFile(file)
	}
	return MCPSearchResult{
		Count: result.Count, Files: files, Offset: result.Offset,
		PageSize: result.PageSize, Order: result.Order, IsAsc: result.IsAsc,
	}
}

func searchManyCallResult(response SearchManyResult) (*mcp.CallToolResult, SearchManyResult, error) {
	return mcpTypedJSONResult("search_many", response, response, response.Failed > 0)
}

func (st *SearchTools) searchMany(ctx context.Context, req *mcp.CallToolRequest, args SearchManyArgs) (*mcp.CallToolResult, SearchManyResult, error) {
	prepared, err := prepareMCPSearchBatch(args)
	if err != nil {
		return toolError(fmt.Sprintf("search_many preflight failed: %v", err)), SearchManyResult{}, nil
	}
	if st.client == nil {
		return toolError("115 client is unavailable"), SearchManyResult{}, nil
	}

	response := SearchManyResult{Requested: len(prepared), Items: make([]SearchManyItemResult, len(prepared))}
	for i, item := range prepared {
		entry := SearchManyItemResult{Index: i, Offset: item.args.Offset, Limit: item.limit}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		result, err := st.client.Search(&driver.SearchOption{
			SearchValue: item.args.SearchValue,
			Offset:      item.args.Offset,
			Limit:       item.limit,
			Type:        item.args.Type,
			Order:       item.args.Order,
			Asc:         item.args.Asc,
		})
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		typed := mcpSearchResultFromDriver(result)
		entry.Data = &typed
		entry.Returned = len(typed.Files)
		if entry.Returned > 0 {
			next := typed.Offset + entry.Returned
			if next < typed.Count {
				entry.NextOffset = &next
			}
		}
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return searchManyCallResult(response)
}
