package server

import "github.com/SheltonZhu/115driver/mcp/server/tools"

// DownloadTransferConfig is the machine-wide configuration for MCP download_file.
type DownloadTransferConfig = tools.DownloadTransferConfig

func DefaultDownloadTransferConfig() DownloadTransferConfig {
	return tools.DefaultDownloadTransferConfig()
}
