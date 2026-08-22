package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func statClientForBody(body string) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	return New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
}

func TestStatRejectsBlankIDBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("blank stat id unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	if info, err := client.Stat("  "); info != nil || !errors.Is(err, ErrWrongParams) {
		t.Fatalf("Stat blank id = %#v, %v; want nil, ErrWrongParams", info, err)
	}
}

func TestStatRejectsSuccessfulResponseWithoutFileCategory(t *testing.T) {
	info, err := statClientForBody(`{"file_name":"ambiguous"}`).Stat("123")
	if info != nil || !errors.Is(err, ErrUnexpected) {
		t.Fatalf("Stat missing file category = %#v, %v; want nil, ErrUnexpected", info, err)
	}
}

func TestStatRejectsNullOrInvalidFileCategory(t *testing.T) {
	for name, body := range map[string]string{
		"null":    `{"file_name":"ambiguous","file_category":null}`,
		"invalid": `{"file_name":"ambiguous","file_category":"2"}`,
	} {
		t.Run(name, func(t *testing.T) {
			info, err := statClientForBody(body).Stat("123")
			if info != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("Stat invalid file category = %#v, %v; want nil, ErrUnexpected", info, err)
			}
		})
	}
}

func TestStatAcceptsLegacySuccessWithoutStateAndSkipsNilParents(t *testing.T) {
	client := statClientForBody(`{"file_name":"folder","pick_code":"pick","count":"2","folder_count":"1","ptime":"1","utime":"2","file_category":"0","paths":[null,{"file_id":"9","file_name":"parent"}]}`)
	info, err := client.Stat("123")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "folder" || info.PickCode != "pick" || !info.IsDirectory || info.FileCount != 2 || info.DirCount != 1 {
		t.Fatalf("unexpected stat info: %#v", info)
	}
	if len(info.Parents) != 1 || info.Parents[0].ID != "9" || info.Parents[0].Name != "parent" {
		t.Fatalf("nil parent was not safely skipped: %#v", info.Parents)
	}
}

func TestStatRejectsExplicitErrorResponse(t *testing.T) {
	client := statClientForBody(`{"state":false,"errno":"20009","error":"missing"}`)
	info, err := client.Stat("missing-id")
	if info != nil || err == nil {
		t.Fatalf("Stat explicit error = %#v, %v; want nil and error", info, err)
	}
}

func TestCheckErrRejectsNilResponseInsteadOfPanicking(t *testing.T) {
	err := CheckErr(nil, noResultError{}, nil)
	if !errors.Is(err, ErrUnexpected) {
		t.Fatalf("CheckErr nil response = %v, want ErrUnexpected", err)
	}
}
