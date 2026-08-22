package tools

import (
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestRedactShareReceiveCodeRemovesAllOccurrences(t *testing.T) {
	got := redactShareReceiveCode("failed secret-code; retry secret-code", " secret-code ")
	if strings.Contains(got, "secret-code") || strings.Count(got, "[REDACTED]") != 2 {
		t.Fatalf("share receive code was not fully redacted: %q", got)
	}
	if got := redactShareReceiveCode("unchanged", " "); got != "unchanged" {
		t.Fatalf("blank receive code changed text: %q", got)
	}
}

func TestValidateSharePaginationRejectsNegativeValues(t *testing.T) {
	for name, values := range map[string][2]int{
		"negative-offset": {-1, 0},
		"negative-limit":  {0, -1},
		"oversized-limit": {0, maxMCPShareBatchLimit + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSharePagination(values[0], values[1]); err == nil {
				t.Fatal("expected invalid pagination error")
			}
		})
	}
	if err := validateSharePagination(5, 0); err != nil {
		t.Fatalf("valid default-limit pagination rejected: %v", err)
	}
}

func TestMarshalShareSnapResultRemovesReceiveCodeWithoutMutatingDriverResponse(t *testing.T) {
	response := &driver.ShareSnapResp{}
	response.Data.Userinfo.UserName = "owner"
	response.Data.Userinfo.Face = "https://avatar.example.invalid/u?token=face-secret"
	response.Data.Shareinfo.ShareTitle = "dataset"
	response.Data.Shareinfo.ReceiveCode = "top-secret-receive-code"
	response.Data.Count = 1
	response.Data.List = []driver.ShareFile{{FileID: "f1", FileName: "a.bin", IsFile: 1, ThumbURL: "https://thumb.example.invalid/u?token=thumb-secret"}}

	raw, err := marshalShareSnapResult(response)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"top-secret-receive-code", "receive_code", "face-secret", "thumb-secret", `"face"`, `"u"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("MCP share result leaked %q: %s", forbidden, text)
		}
	}
	for _, expected := range []string{"dataset", "owner", "a.bin", "f1"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("MCP share result lost %q: %s", expected, text)
		}
	}
	if response.Data.Shareinfo.ReceiveCode != "top-secret-receive-code" {
		t.Fatalf("sanitization mutated driver response: %q", response.Data.Shareinfo.ReceiveCode)
	}
	if response.Data.Userinfo.Face == "" || response.Data.List[0].ThumbURL == "" {
		t.Fatal("share sanitization mutated avatar/thumbnail URLs in driver response")
	}
	if _, err := marshalShareSnapResult(nil); err == nil {
		t.Fatal("nil share snapshot was accepted")
	}
}

func TestBuildMCPShareSnapOutputRedactsReceiveCodeAcrossAllValues(t *testing.T) {
	const receiveCode = "secret-code"
	response := &driver.ShareSnapResp{}
	response.Data.Userinfo.UserName = "owner-" + receiveCode
	response.Data.Shareinfo.ShareTitle = "dataset-" + receiveCode
	response.Data.Shareinfo.ReceiveCode = receiveCode
	response.Data.Count = 1
	response.Data.List = []driver.ShareFile{{FileID: "f1", FileName: "file-" + receiveCode, IsFile: 1}}

	text, output, err := buildMCPShareSnapOutput(response, receiveCode)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, receiveCode) || strings.Contains(text, "receive_code") {
		t.Fatalf("redacted share text leaked receive code: %s", text)
	}
	if strings.Contains(output.Data.UserInfo.UserName, receiveCode) || strings.Contains(output.Data.ShareInfo.ShareTitle, receiveCode) || strings.Contains(output.Data.List[0].FileName, receiveCode) {
		t.Fatalf("typed share output leaked receive code: %#v", output)
	}
	if output.Data.ShareInfo.ShareTitle != "dataset-[REDACTED]" || output.Data.List[0].FileName != "file-[REDACTED]" {
		t.Fatalf("unexpected redacted typed share output: %#v", output)
	}
}
