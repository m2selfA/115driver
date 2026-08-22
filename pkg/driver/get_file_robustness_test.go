package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestGetFileRejectsBlankIDBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("blank GetFile id unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	if file, err := client.GetFile("  "); file != nil || !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetFile blank id = %#v, %v; want nil, ErrWrongParams", file, err)
	}
}

func TestGetFileRejectsSuccessfulEmptyData(t *testing.T) {
	for name, body := range map[string]string{
		"empty":     `{"state":true,"data":[]}`,
		"nil-entry": `{"state":true,"data":[null]}`,
	} {
		t.Run(name, func(t *testing.T) {
			transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			})
			client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
			file, err := client.GetFile("missing-id")
			if file != nil || !errors.Is(err, ErrNotExist) {
				t.Fatalf("GetFile malformed success = %#v, %v; want nil, ErrNotExist", file, err)
			}
		})
	}
}

func TestGetFileRejectsMismatchedOrMalformedIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"mismatched-file": `{"state":true,"data":[{"fid":"other","cid":"0","n":"file.bin","s":"1"}]}`,
		"mismatched-dir":  `{"state":true,"data":[{"cid":"other","n":"dir","s":"0"}]}`,
		"missing-id":      `{"state":true,"data":[{"n":"ambiguous","s":"0"}]}`,
		"negative-size":   `{"state":true,"data":[{"fid":"target","cid":"0","n":"file.bin","s":"-1"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := metadataClientForBody(body)
			file, err := client.GetFile("target")
			if file != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("GetFile malformed identity = %#v, %v; want nil, ErrUnexpected", file, err)
			}
		})
	}

	file, err := metadataClientForBody(`{"state":true,"data":[{"fid":"target","cid":"0","n":"file.bin","s":"1"}]}`).GetFile("target")
	if err != nil || file == nil || file.FileID != "target" {
		t.Fatalf("GetFile valid identity = %#v, %v", file, err)
	}
}

func TestFileFromSkipsNilLabels(t *testing.T) {
	info := &FileInfo{
		FileID: "f1",
		Labels: []*LabelInfo{nil, {ID: "l1", Name: "label", Color: "#FF0000"}},
	}
	file := (&File{}).From(info)
	if file == nil || len(file.Labels) != 1 || file.Labels[0] == nil || file.Labels[0].ID != "l1" {
		t.Fatalf("File.From labels = %#v", file)
	}
}

func TestFileFromNilIsNoOp(t *testing.T) {
	file := &File{FileID: "existing", Name: "existing.bin"}
	if got := file.From(nil); got != file {
		t.Fatalf("File.From(nil) returned %#v, want original receiver %#v", got, file)
	}
	if file.FileID != "existing" || file.Name != "existing.bin" {
		t.Fatalf("File.From(nil) mutated receiver: %#v", file)
	}
}
