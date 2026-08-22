package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestDownloadByShareCodeRejectsBlankIdentityBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("blank share identity unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	for name, tc := range map[string]struct{ shareCode, fileID string }{
		"share-code": {shareCode: " ", fileID: "file-id"},
		"file-id":    {shareCode: "share-code", fileID: " "},
	} {
		t.Run(name, func(t *testing.T) {
			if info, err := client.DownloadByShareCode(tc.shareCode, "", tc.fileID); info != nil || !errors.Is(err, ErrWrongParams) {
				t.Fatalf("DownloadByShareCode blank identity = %#v, %v; want nil, ErrWrongParams", info, err)
			}
		})
	}
}

func TestDownloadByShareCodePreservesDownloadRequestContext(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Query().Get("share_code"); got != "share-code" {
			t.Fatalf("share_code = %q", got)
		}
		if got := req.URL.Query().Get("receive_code"); got != "receive-code" {
			t.Fatalf("receive_code = %q", got)
		}
		if got := req.URL.Query().Get("file_id"); got != "file-id" {
			t.Fatalf("file_id = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type": {"application/json"},
				"Set-Cookie":   {"download_token=abc; Path=/"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"state":true,"data":{"fid":"file-id","fn":"shared.bin","fs":"12","url":{"url":"https://cdn.example.invalid/shared.bin"}}}`)),
			Request: req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))

	info, err := client.DownloadByShareCodeRequestWithUA("share-test-agent", "share-code", "receive-code", "file-id")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.FileID != "file-id" || info.FileName != "shared.bin" || int64(info.FileSize) != 12 || info.URL.URL == "" {
		t.Fatalf("unexpected share download info: %#v", info)
	}
	if got := info.Header.Get("Referer"); got != BuildShareReferer("share-code", "receive-code") {
		t.Fatalf("download referer = %q", got)
	}
	if got := info.Header.Get("User-Agent"); got != "share-test-agent" {
		t.Fatalf("download user-agent = %q", got)
	}
	if got := info.Header.Get("Cookie"); !strings.Contains(got, "download_token=abc") {
		t.Fatalf("download cookie context = %q", got)
	}
}

func TestDownloadByShareCodeRejectsMalformedSuccessfulMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"empty-url":     {body: `{"state":true,"data":{"fid":"file-id","fn":"shared.bin","fs":"12","url":{"url":""}}}`, want: ErrUnexpected},
		"negative-size": {body: `{"state":true,"data":{"fid":"file-id","fn":"shared.bin","fs":"-1","url":{"url":"https://cdn.example.invalid/shared.bin"}}}`, want: ErrDownloadEmpty},
		"mismatched-id": {body: `{"state":true,"data":{"fid":"other-id","fn":"shared.bin","fs":"12","url":{"url":"https://cdn.example.invalid/shared.bin"}}}`, want: ErrUnexpected},
	} {
		t.Run(name, func(t *testing.T) {
			transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
					Request:    req,
				}, nil
			})
			client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
			info, err := client.DownloadByShareCode("share-code", "receive-code", "file-id")
			if info != nil || !errors.Is(err, tc.want) {
				t.Fatalf("DownloadByShareCode malformed success = %#v, %v; want nil, %v", info, err, tc.want)
			}
		})
	}
}
