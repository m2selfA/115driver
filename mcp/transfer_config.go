package main

import (
	"github.com/SheltonZhu/115driver/internal/mcpapp"
	"github.com/SheltonZhu/115driver/mcp/server"
)

func readDownloadTransferConfig(configPath string) (server.DownloadTransferConfig, error) {
	return mcpapp.ReadDownloadTransferConfig(configPath)
}
