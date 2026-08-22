package driver

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateUploadCompletionMetadataRequiresConsistentIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		result    *UploadResult
		sha1      string
		size      int64
		wantCheck bool
		wantErr   error
	}{
		"nil-result":      {result: nil, sha1: "ABC", size: 3, wantErr: ErrUnexpected},
		"bad-expectation": {result: &UploadResult{}, sha1: " ", size: 3, wantErr: ErrWrongParams},
		"sha1-mismatch":   {result: uploadCompletionResult("f1", "OTHER", 3), sha1: "ABC", size: 3, wantErr: ErrUnexpected},
		"size-mismatch":   {result: uploadCompletionResult("f1", "ABC", 2), sha1: "ABC", size: 3, wantErr: ErrUnexpected},
		"empty-data":      {result: &UploadResult{}, sha1: "ABC", size: 3, wantCheck: true},
		"missing-size":    {result: uploadCompletionResult("f1", "ABC", 0), sha1: "ABC", size: 3, wantCheck: true},
		"complete":        {result: uploadCompletionResult("f1", "abc", 3), sha1: "ABC", size: 3},
	} {
		t.Run(name, func(t *testing.T) {
			needsCheck, err := validateUploadCompletionMetadata(tc.result, tc.sha1, tc.size)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("validateUploadCompletionMetadata = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || needsCheck != tc.wantCheck {
				t.Fatalf("validateUploadCompletionMetadata = check %v, err %v; want check %v", needsCheck, err, tc.wantCheck)
			}
		})
	}
}

func uploadCompletionResult(fileID, sha1 string, size int) *UploadResult {
	result := &UploadResult{}
	result.Data.FileID = fileID
	result.Data.Sha1 = sha1
	result.Data.FileSize = size
	return result
}

func TestUploadResultPreservesVerificationCode10002(t *testing.T) {
	body := `{"state":false,"message":"校验文件失败，请重新上传。","code":10002}`
	resp := UploadResult{BasicResp: BasicResp{State: false, Code: 10002, Message: "校验文件失败，请重新上传。"}}
	err := resp.Err(body)
	if !errors.Is(err, ErrUploadVerificationFailed) {
		t.Fatalf("expected code 10002 to map to upload verification failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "10002") {
		t.Fatalf("expected original response body to be preserved, got %v", err)
	}
}
