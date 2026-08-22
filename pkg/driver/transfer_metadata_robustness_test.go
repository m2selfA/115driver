package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestGetOSSTokenRejectsMalformedSuccessfulToken(t *testing.T) {
	for name, body := range map[string]string{
		"missing-access-key": `{"StatusCode":"200","AccessKeySecret":"secret","SecurityToken":"sts"}`,
		"missing-secret":     `{"StatusCode":"200","AccessKeyID":"ak","SecurityToken":"sts"}`,
		"missing-sts":        `{"StatusCode":"200","AccessKeyID":"ak","AccessKeySecret":"secret"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := metadataClientForBody(body)
			token, err := client.GetOSSToken()
			if token != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("GetOSSToken malformed success = %#v, %v; want nil, ErrUnexpected", token, err)
			}
		})
	}
}

func TestGetOSSTokenReturnsNilOnProtocolError(t *testing.T) {
	client := metadataClientForBody(`{"StatusCode":"403"}`)
	token, err := client.GetOSSToken()
	if token != nil || err == nil {
		t.Fatalf("GetOSSToken error response = %#v, %v; want nil and error", token, err)
	}
}

func TestGetOSSTokenAcceptsUsableToken(t *testing.T) {
	client := metadataClientForBody(`{"StatusCode":"200","AccessKeyID":"ak","AccessKeySecret":"secret","SecurityToken":"sts"}`)
	token, err := client.GetOSSToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == nil || token.AccessKeyID != "ak" || token.AccessKeySecret != "secret" || token.SecurityToken != "sts" {
		t.Fatalf("unexpected OSS token: %#v", token)
	}
}

func TestValidateDownloadInfoRejectsUnusableMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		info        *DownloadInfo
		requireSize bool
		want        error
	}{
		"nil":           {info: nil, want: ErrUnexpected},
		"negative-size": {info: &DownloadInfo{FileSize: -1, Url: FileDownloadUrl{Valid: true, Url: "https://cdn.invalid/file"}}, requireSize: true, want: ErrDownloadEmpty},
		"invalid-url":   {info: &DownloadInfo{FileSize: 1, Url: FileDownloadUrl{Url: "https://cdn.invalid/file"}}, requireSize: true, want: ErrUnexpected},
		"empty-url":     {info: &DownloadInfo{FileSize: 1, Url: FileDownloadUrl{Valid: true}}, requireSize: true, want: ErrUnexpected},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDownloadInfo(tc.info, tc.requireSize); !errors.Is(err, tc.want) {
				t.Fatalf("validateDownloadInfo = %v, want %v", err, tc.want)
			}
		})
	}
	if err := validateDownloadInfo(&DownloadInfo{Url: FileDownloadUrl{Valid: true, Url: "https://cdn.invalid/file"}}, false); err != nil {
		t.Fatalf("Android-style unknown-size metadata rejected: %v", err)
	}
}

func TestSelectDownloadInfoRejectsAmbiguousOrMismatchedMetadata(t *testing.T) {
	usable := func(pickCode string) *DownloadInfo {
		return &DownloadInfo{PickCode: pickCode, FileSize: 1, Url: FileDownloadUrl{Valid: true, Url: "https://cdn.invalid/file"}}
	}

	for name, tc := range map[string]struct {
		data DownloadData
		pick string
		want error
	}{
		"empty-request":            {data: DownloadData{"one": usable("p1")}, pick: " ", want: ErrPickCodeIsEmpty},
		"empty-data":               {data: DownloadData{}, pick: "p1", want: ErrUnexpected},
		"single-explicit-mismatch": {data: DownloadData{"one": usable("other")}, pick: "p1", want: ErrUnexpected},
		"multiple-no-match":        {data: DownloadData{"one": usable("a"), "two": usable("b")}, pick: "p1", want: ErrUnexpected},
		"multiple-with-nil":        {data: DownloadData{"one": usable("a"), "two": nil}, pick: "p1", want: ErrUnexpected},
	} {
		t.Run(name, func(t *testing.T) {
			info, err := selectDownloadInfo(tc.data, tc.pick)
			if info != nil || !errors.Is(err, tc.want) {
				t.Fatalf("selectDownloadInfo = %#v, %v; want nil, %v", info, err, tc.want)
			}
		})
	}

	legacy := usable("")
	if info, err := selectDownloadInfo(DownloadData{"legacy-key": legacy}, "p1"); err != nil || info != legacy {
		t.Fatalf("single legacy metadata rejected: %#v, %v", info, err)
	}
	matched := usable("p1")
	if info, err := selectDownloadInfo(DownloadData{"one": usable("other"), "two": matched}, "p1"); err != nil || info != matched {
		t.Fatalf("multiple metadata did not select requested pickcode: %#v, %v", info, err)
	}
}

func TestDownloadRejectsBlankPickCodeBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("blank pickcode unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	if info, err := client.DownloadWithUA("  ", ""); info != nil || !errors.Is(err, ErrPickCodeIsEmpty) {
		t.Fatalf("DownloadWithUA blank pickcode = %#v, %v", info, err)
	}
	if info, err := client.DownloadWithUAByAndroidAPI("  ", ""); info != nil || !errors.Is(err, ErrPickCodeIsEmpty) {
		t.Fatalf("DownloadWithUAByAndroidAPI blank pickcode = %#v, %v", info, err)
	}
}

func metadataClientForBody(body string) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	return New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
}
