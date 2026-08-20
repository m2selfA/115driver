package driver

import (
	"errors"
	"strings"
	"testing"
)

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
