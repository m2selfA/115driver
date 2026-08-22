package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SheltonZhu/115driver/internal/accountinfo"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type AccountTools struct {
	client *driver.Pan115Client
}

// MCPAccountInfo is the stable account shape exposed to MCP clients. The raw
// imei_info payload is intentionally omitted; it is not needed for account or
// storage reporting and may contain device identifiers.
type MCPAccountInfo struct {
	User         accountinfo.User        `json:"user"`
	Space        accountinfo.Space       `json:"space"`
	LoginDevices driver.LoginDevicesInfo `json:"login_devices"`
}

// MCPAppVersion is stable remote service metadata, distinct from the local
// 115driver/MCP server build version.
type MCPAppVersion struct {
	App     string `json:"app" jsonschema:"115 client application identifier"`
	Version string `json:"version" jsonschema:"currently advertised application version; may be empty when upstream metadata is malformed"`
}

// MCPAppVersionsResult is the typed app-version service response.
type MCPAppVersionsResult struct {
	Count    int             `json:"count" jsonschema:"number of advertised application records"`
	Versions []MCPAppVersion `json:"versions" jsonschema:"application versions sorted by application identifier"`
}

func mcpAccountInfo(info accountinfo.AccountInfo) MCPAccountInfo {
	return MCPAccountInfo{User: info.User, Space: info.Space, LoginDevices: info.LoginDevices}
}

func NewAccountTools(client *driver.Pan115Client) *AccountTools {
	return &AccountTools{
		client: client,
	}
}

func (at *AccountTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "getAccountInfo",
		Description: "Get current account, storage space, and login device info",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, at.getAccountInfo)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_app_versions",
		Description: "Get currently advertised 115 client application versions from the remote version service",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, at.getAppVersions)
}

func appVersionsCallResult(response MCPAppVersionsResult) (*mcp.CallToolResult, MCPAppVersionsResult, error) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize app versions: %v", err)), MCPAppVersionsResult{}, nil
	}
	return mcpTypedTextResult(string(encoded), response, false)
}

func (at *AccountTools) getAppVersions(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, MCPAppVersionsResult, error) {
	if at.client == nil {
		return toolError("115 client is unavailable"), MCPAppVersionsResult{}, nil
	}
	versions, err := at.client.GetAppVersion()
	if err != nil {
		return toolError(fmt.Sprintf("Failed to get 115 app versions: %v", err)), MCPAppVersionsResult{}, nil
	}
	response := MCPAppVersionsResult{Count: len(versions), Versions: make([]MCPAppVersion, len(versions))}
	for i, version := range versions {
		response.Versions[i] = MCPAppVersion{App: version.AppName, Version: version.Version}
	}
	return appVersionsCallResult(response)
}

func (at *AccountTools) getAccountInfo(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, MCPAccountInfo, error) {
	userInfo, err := at.client.GetUser()
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to get user info: %v", err),
				},
			},
			IsError: true,
		}, MCPAccountInfo{}, nil
	}
	info, err := at.client.GetInfo()
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to get account info: %v", err),
				},
			},
			IsError: true,
		}, MCPAccountInfo{}, nil
	}

	account := accountinfo.FromDriverData(userInfo, info)
	typed := mcpAccountInfo(account)
	responseJSON, err := marshalAccountInfoResult(account)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize account info: %v", err),
				},
			},
			IsError: true,
		}, MCPAccountInfo{}, nil
	}

	return mcpTypedTextResult(responseJSON, typed, false)
}

func marshalAccountInfoResult(info accountinfo.AccountInfo) (string, error) {
	responseJSON, err := json.Marshal(mcpAccountInfo(info))
	if err != nil {
		return "", err
	}
	return string(responseJSON), nil
}
