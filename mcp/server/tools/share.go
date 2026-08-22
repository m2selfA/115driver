package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ShareTools holds share-related MCP tools
type ShareTools struct {
	client *driver.Pan115Client
}

// NewShareTools creates a new ShareTools instance
func NewShareTools(client *driver.Pan115Client) *ShareTools {
	return &ShareTools{
		client: client,
	}
}

// GetShareSnapArgs defines arguments for get share snap tool
type GetShareSnapArgs struct {
	ShareCode   string `json:"share_code" jsonschema:"required,share code"`
	ReceiveCode string `json:"receive_code" jsonschema:"required,receive code"`
	DirID       string `json:"dir_id" jsonschema:"directory ID to list, default is root directory"`
	Offset      int    `json:"offset,omitempty" jsonschema:"offset for pagination, default is 0"`
	Limit       int    `json:"limit,omitempty" jsonschema:"number of items to return, default is 20, maximum is 500"`
}

// MCPShareSnapOutput is the stable, credential-free typed share snapshot.
type MCPShareSnapOutput struct {
	Data MCPShareSnapData `json:"data"`
}

type MCPShareSnapData struct {
	UserInfo   MCPShareUserInfo   `json:"userinfo"`
	ShareInfo  MCPShareInfo       `json:"shareinfo"`
	Count      int                `json:"count"`
	List       []MCPShareFile     `json:"list"`
	ShareState int64              `json:"share_state"`
	UserAppeal MCPShareUserAppeal `json:"user_appeal"`
}

type MCPShareUserInfo struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

type MCPShareInfo struct {
	SnapID           string `json:"snap_id"`
	FileSize         int64  `json:"file_size"`
	ShareTitle       string `json:"share_title"`
	ShareState       int64  `json:"share_state"`
	ForbidReason     string `json:"forbid_reason"`
	CreateTime       int64  `json:"create_time"`
	ReceiveCount     int64  `json:"receive_count"`
	ExpireTime       int64  `json:"expire_time"`
	FileCategory     int64  `json:"file_category"`
	AutoRenewal      int64  `json:"auto_renewal"`
	ShareDuration    int    `json:"share_duration"`
	AutoFillRecvcode int64  `json:"auto_fill_recvcode"`
	CanReport        int    `json:"can_report"`
	CanNotice        int    `json:"can_notice"`
	HaveVioFile      int    `json:"have_vio_file"`
	SkipLoginState   int64  `json:"skip_login_state"`
}

type MCPShareFile struct {
	FileID      string `json:"fid"`
	UID         int    `json:"uid"`
	CategoryID  string `json:"cid"`
	FileName    string `json:"n"`
	Type        string `json:"ico"`
	SHA1        string `json:"sha"`
	Size        int64  `json:"s"`
	UpdateTime  string `json:"t"`
	IsFile      int    `json:"fc"`
	ParentID    string `json:"pid"`
	IsSkipLogin int    `json:"is_skip_login"`
}

type MCPShareUserAppeal struct {
	CanAppeal       int `json:"can_appeal"`
	CanShareAppeal  int `json:"can_share_appeal"`
	PopupAppealPage int `json:"popup_appeal_page"`
	CanGlobalAppeal int `json:"can_global_appeal"`
}

func buildMCPShareSnapOutput(result *driver.ShareSnapResp, receiveCode string) (string, MCPShareSnapOutput, error) {
	raw, err := marshalShareSnapResult(result)
	if err != nil {
		return "", MCPShareSnapOutput{}, err
	}
	text := redactShareReceiveCode(string(raw), receiveCode)
	var output MCPShareSnapOutput
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return "", MCPShareSnapOutput{}, err
	}
	return text, output, nil
}

// RegisterTools registers share-related tools with the MCP server
func (st *ShareTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "getShareSnap",
		Description: "Get shared files and directories snapshot information",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, st.getShareSnap)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_share_snaps",
		Description: "List multiple independently paginated 115 share snapshots in one bounded read-only batch without echoing share credentials",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, st.getShareSnaps)
}

func marshalShareSnapResult(result *driver.ShareSnapResp) ([]byte, error) {
	if result == nil {
		return nil, fmt.Errorf("share snapshot result is nil")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return raw, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(root["data"], &data); err != nil {
		return nil, err
	}
	var shareInfo map[string]json.RawMessage
	if err := json.Unmarshal(data["shareinfo"], &shareInfo); err != nil {
		return nil, err
	}
	delete(shareInfo, "receive_code")
	shareInfoJSON, err := json.Marshal(shareInfo)
	if err != nil {
		return nil, err
	}
	data["shareinfo"] = shareInfoJSON

	// Avatar and thumbnail URLs are display-only metadata and may carry
	// short-lived CDN query tokens. Keep default MCP share inspection URL-free.
	if rawUserInfo, ok := data["userinfo"]; ok {
		var userInfo map[string]json.RawMessage
		if err := json.Unmarshal(rawUserInfo, &userInfo); err != nil {
			return nil, err
		}
		delete(userInfo, "face")
		userInfoJSON, err := json.Marshal(userInfo)
		if err != nil {
			return nil, err
		}
		data["userinfo"] = userInfoJSON
	}
	if rawList, ok := data["list"]; ok {
		var list []map[string]json.RawMessage
		if err := json.Unmarshal(rawList, &list); err != nil {
			return nil, err
		}
		for i := range list {
			delete(list[i], "u")
		}
		listJSON, err := json.Marshal(list)
		if err != nil {
			return nil, err
		}
		data["list"] = listJSON
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	root["data"] = dataJSON
	return json.Marshal(root)
}

func redactShareReceiveCode(text, receiveCode string) string {
	receiveCode = strings.TrimSpace(receiveCode)
	if receiveCode == "" {
		return text
	}
	return strings.ReplaceAll(text, receiveCode, "[REDACTED]")
}

func validateSharePagination(offset, limit int) error {
	if offset < 0 {
		return fmt.Errorf("offset must not be negative")
	}
	if limit < 0 {
		return fmt.Errorf("limit must not be negative")
	}
	if limit > maxMCPShareBatchLimit {
		return fmt.Errorf("limit must not exceed %d", maxMCPShareBatchLimit)
	}
	return nil
}

func (st *ShareTools) getShareSnap(ctx context.Context, req *mcp.CallToolRequest, args GetShareSnapArgs) (*mcp.CallToolResult, MCPShareSnapOutput, error) {
	if err := validateSharePagination(args.Offset, args.Limit); err != nil {
		return toolError(fmt.Sprintf("Invalid share pagination: %v", err)), MCPShareSnapOutput{}, nil
	}
	var (
		result *driver.ShareSnapResp
		err    error
	)

	// Prepare queries
	queries := make([]driver.Query, 0)
	if args.Limit > 0 {
		queries = append(queries, driver.QueryLimit(args.Limit))
	}
	if args.Offset > 0 {
		queries = append(queries, driver.QueryOffset(args.Offset))
	}

	result, err = st.client.GetShareSnap(args.ShareCode, args.ReceiveCode, args.DirID, queries...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: redactShareReceiveCode(fmt.Sprintf("Failed to get share snap: %v", err), args.ReceiveCode),
				},
			},
			IsError: true,
		}, MCPShareSnapOutput{}, nil
	}

	// Build TextContent and structured output from the same redacted JSON. This
	// removes the receive_code field and redacts accidental occurrences in any
	// other string value (title, file name, error text, etc.).
	resultText, output, err := buildMCPShareSnapOutput(result, args.ReceiveCode)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, MCPShareSnapOutput{}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: resultText,
			},
		},
	}, output, nil
}
