package tools

import (
	"context"
	"testing"
)

func TestMCPFileBatchToolsRejectEmptyAndOverLimitBeforeClientUse(t *testing.T) {
	ft := NewFileTools(nil)

	if result, _, err := ft.downloadFiles(context.Background(), nil, DownloadFilesArgs{}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty download_files = %#v, %v", result, err)
	}
	if result, _, err := ft.downloadShareFiles(context.Background(), nil, DownloadShareFilesArgs{ShareCode: "share"}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty download_share_files = %#v, %v", result, err)
	}
	if result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty upload_from_local_files = %#v, %v", result, err)
	}
	if result, _, err := ft.uploadFromURLs(context.Background(), nil, UploadFromURLFilesArgs{}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty upload_from_urls = %#v, %v", result, err)
	}

	if result, _, err := ft.downloadFiles(context.Background(), nil, DownloadFilesArgs{Files: make([]DownloadFileArgs, maxMCPFileBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized download_files = %#v, %v", result, err)
	}
	if result, _, err := ft.downloadShareFiles(context.Background(), nil, DownloadShareFilesArgs{ShareCode: "share", Files: make([]DownloadShareFilesItem, maxMCPFileBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized download_share_files = %#v, %v", result, err)
	}
	if result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: make([]UploadFromLocalFileItem, maxMCPFileBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized upload_from_local_files = %#v, %v", result, err)
	}
	if result, _, err := ft.uploadFromURLs(context.Background(), nil, UploadFromURLFilesArgs{Files: make([]UploadFromURLFileItem, maxMCPFileBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized upload_from_urls = %#v, %v", result, err)
	}
}
