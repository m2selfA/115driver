package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPShareBatchItems   = 256
	defaultMCPShareLimit    = 20
	maxMCPShareBatchLimit   = 500
	maxMCPShareBatchEntries = 5000
)

// GetShareSnapsItem defines one independently paginated share listing request.
// Share/receive codes are control-plane inputs only and are never echoed in output.
type GetShareSnapsItem struct {
	ShareCode   string `json:"share_code" jsonschema:"share code"`
	ReceiveCode string `json:"receive_code" jsonschema:"share receive code/password; may be empty for shares that do not require one"`
	DirID       string `json:"dir_id,omitempty" jsonschema:"directory ID inside the share; empty means root 0"`
	Offset      int    `json:"offset,omitempty" jsonschema:"zero-based page offset, default 0"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size, default 20, maximum 500"`
}

// GetShareSnapsArgs defines a bounded multi-share listing.
type GetShareSnapsArgs struct {
	Requests []GetShareSnapsItem `json:"requests" jsonschema:"independently paginated share listing requests; exact logical duplicates are rejected"`
}

// GetShareSnapsItemResult reports one share page without echoing credentials.
type GetShareSnapsItemResult struct {
	Index      int                 `json:"index" jsonschema:"zero-based input request index"`
	DirID      string              `json:"dir_id" jsonschema:"normalized directory ID inside the share"`
	Offset     int                 `json:"offset" jsonschema:"requested page offset"`
	Limit      int                 `json:"limit" jsonschema:"effective page size"`
	Returned   int                 `json:"returned" jsonschema:"number of returned entries"`
	NextOffset *int                `json:"next_offset,omitempty" jsonschema:"continuation offset when another page may exist"`
	Success    bool                `json:"success" jsonschema:"whether this share page was listed successfully"`
	Error      string              `json:"error,omitempty" jsonschema:"sanitized item error when listing failed"`
	Data       *MCPShareSnapOutput `json:"data,omitempty" jsonschema:"credential-free share snapshot when successful"`
}

// GetShareSnapsResult summarizes a bounded share-list batch.
type GetShareSnapsResult struct {
	Requested int                       `json:"requested" jsonschema:"number of requested share pages"`
	Succeeded int                       `json:"succeeded" jsonschema:"number of successful share pages"`
	Failed    int                       `json:"failed" jsonschema:"number of failed share pages"`
	Items     []GetShareSnapsItemResult `json:"items" jsonschema:"per-request results in input order"`
}

type mcpPreparedShareSnap struct {
	shareCode   string
	receiveCode string
	dirID       string
	offset      int
	limit       int
}

type mcpShareSnapBatchKey struct {
	ShareCode   string
	ReceiveCode string
	DirID       string
	Offset      int
	Limit       int
}

func prepareMCPShareSnapBatch(args GetShareSnapsArgs) ([]mcpPreparedShareSnap, error) {
	if len(args.Requests) == 0 {
		return nil, fmt.Errorf("at least one share listing request is required")
	}
	if len(args.Requests) > maxMCPShareBatchItems {
		return nil, fmt.Errorf("received %d share listing requests; maximum is %d", len(args.Requests), maxMCPShareBatchItems)
	}

	prepared := make([]mcpPreparedShareSnap, len(args.Requests))
	seen := make(map[mcpShareSnapBatchKey]int, len(args.Requests))
	entryBudget := 0
	for i, item := range args.Requests {
		shareCode := strings.TrimSpace(item.ShareCode)
		if shareCode == "" {
			return nil, fmt.Errorf("share listing request %d has an empty share_code", i)
		}
		if err := validateSharePagination(item.Offset, item.Limit); err != nil {
			return nil, fmt.Errorf("share listing request %d pagination: %w", i, err)
		}
		limit := item.Limit
		if limit == 0 {
			limit = defaultMCPShareLimit
		}
		if limit > maxMCPShareBatchLimit {
			return nil, fmt.Errorf("share listing request %d limit %d exceeds maximum %d", i, limit, maxMCPShareBatchLimit)
		}
		entryBudget += limit
		if entryBudget > maxMCPShareBatchEntries {
			return nil, fmt.Errorf("share listing batch effective page budget %d exceeds maximum %d", entryBudget, maxMCPShareBatchEntries)
		}
		dirID := strings.TrimSpace(item.DirID)
		if dirID == "" {
			dirID = "0"
		}
		key := mcpShareSnapBatchKey{
			ShareCode: shareCode, ReceiveCode: item.ReceiveCode, DirID: dirID,
			Offset: item.Offset, Limit: limit,
		}
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("share listing requests %d and %d are exact logical duplicates", previous, i)
		}
		seen[key] = i
		prepared[i] = mcpPreparedShareSnap{
			shareCode: shareCode, receiveCode: item.ReceiveCode, dirID: dirID,
			offset: item.Offset, limit: limit,
		}
	}
	return prepared, nil
}

func getShareSnapsCallResult(response GetShareSnapsResult) (*mcp.CallToolResult, GetShareSnapsResult, error) {
	return mcpTypedJSONResult("get_share_snaps", response, response, response.Failed > 0)
}

func (st *ShareTools) getShareSnaps(ctx context.Context, req *mcp.CallToolRequest, args GetShareSnapsArgs) (*mcp.CallToolResult, GetShareSnapsResult, error) {
	prepared, err := prepareMCPShareSnapBatch(args)
	if err != nil {
		return toolError(fmt.Sprintf("get_share_snaps preflight failed: %v", err)), GetShareSnapsResult{}, nil
	}
	if st.client == nil {
		return toolError("115 client is unavailable"), GetShareSnapsResult{}, nil
	}

	response := GetShareSnapsResult{Requested: len(prepared), Items: make([]GetShareSnapsItemResult, len(prepared))}
	for i, item := range prepared {
		entry := GetShareSnapsItemResult{Index: i, DirID: item.dirID, Offset: item.offset, Limit: item.limit}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		result, err := st.client.GetShareSnap(
			item.shareCode, item.receiveCode, item.dirID,
			driver.QueryOffset(item.offset), driver.QueryLimit(item.limit),
		)
		if err != nil {
			entry.Error = redactShareReceiveCode(err.Error(), item.receiveCode)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		_, typed, err := buildMCPShareSnapOutput(result, item.receiveCode)
		if err != nil {
			entry.Error = redactShareReceiveCode(err.Error(), item.receiveCode)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Data = &typed
		entry.Returned = len(typed.Data.List)
		if entry.Returned > 0 {
			next := item.offset + entry.Returned
			if next < typed.Data.Count {
				entry.NextOffset = &next
			}
		}
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return getShareSnapsCallResult(response)
}
