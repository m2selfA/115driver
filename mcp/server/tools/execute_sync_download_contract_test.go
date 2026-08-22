package tools

import (
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestValidateMCPSyncExecutablePlanRequiresDownloadContentSnapshot(t *testing.T) {
	item := syncplanpkg.Item{
		RelativePath:  "remote.bin",
		Action:        "download",
		Kind:          "file",
		RemotePresent: true,
		RemotePath:    "/remote/remote.bin",
		RemoteID:      "remote-id",
		RemoteSize:    4,
	}
	if err := validateMCPSyncExecutablePlan(syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}); err == nil {
		t.Fatal("download without remote content snapshot was accepted")
	}
	item.RemoteSHA1 = mcpSyncTestSHA1("AAAA")
	if err := validateMCPSyncExecutablePlan(syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}); err != nil {
		t.Fatalf("content-bound download was rejected: %v", err)
	}
}

func TestValidateMCPSyncExecutablePlanAllowsDirectoryDownloadWithoutSHA1(t *testing.T) {
	item := syncplanpkg.Item{
		RelativePath:  "dir",
		Action:        "download",
		Kind:          "directory",
		RemotePresent: true,
		RemotePath:    "/remote/dir",
		RemoteID:      "remote-dir-id",
	}
	if err := validateMCPSyncExecutablePlan(syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}); err != nil {
		t.Fatalf("directory download incorrectly required content digest: %v", err)
	}
}
