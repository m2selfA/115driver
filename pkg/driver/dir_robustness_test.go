package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestListPublicInputsRejectInvalidValuesBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid list input unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))

	if _, err := client.ListWithLimit("0", 0, nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListWithLimit zero = %v, want ErrWrongParams", err)
	}
	if _, err := client.ListPage("0", -1, 1, nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListPage negative offset = %v, want ErrWrongParams", err)
	}
	if _, err := client.ListPage("0", 0, 0, nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListPage zero limit = %v, want ErrWrongParams", err)
	}
	if _, err := GetFiles(nil, "0"); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetFiles nil request = %v, want ErrWrongParams", err)
	}
	if _, err := GetFiles(client.NewRequest(), "0", WithLimit(0)); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetFiles zero limit = %v, want ErrWrongParams", err)
	}
	if _, err := GetFiles(client.NewRequest(), "0", WithOffset(-1)); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetFiles negative offset = %v, want ErrWrongParams", err)
	}
	if _, err := GetFiles(client.NewRequest(), "0", WithApiURL("")); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetFiles empty endpoint = %v, want ErrWrongParams", err)
	}
}

func TestListNilOptionsAreIgnored(t *testing.T) {
	client := dirClientForBody(`{"state":true,"cid":"42","count":0,"offset":0,"limit":1,"data":[]}`)
	files, err := client.ListPage("42", 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if files == nil || len(*files) != 0 {
		t.Fatalf("ListPage nil option = %#v", files)
	}

	result, err := GetFiles(client.NewRequest(), "42", nil, WithApiURL(ApiFileList), WithLimit(1))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || string(result.CategoryID) != "42" {
		t.Fatalf("GetFiles nil option = %#v", result)
	}
}

func TestGetFilesRejectsMalformedSuccessfulPages(t *testing.T) {
	cases := map[string]struct {
		body   string
		offset int64
		limit  int64
	}{
		"negative-count":  {body: `{"state":true,"cid":"42","count":-1,"offset":0,"data":[]}`, limit: 2},
		"negative-offset": {body: `{"state":true,"cid":"42","count":0,"offset":-1,"data":[]}`, limit: 2},
		"over-limit":      {body: `{"state":true,"cid":"42","count":3,"offset":0,"data":[{"fid":"1","cid":"42","n":"a"},{"fid":"2","cid":"42","n":"b"},{"fid":"3","cid":"42","n":"c"}]}`, limit: 2},
		"offset-mismatch": {body: `{"state":true,"cid":"42","count":3,"offset":0,"data":[{"fid":"2","cid":"42","n":"b"}]}`, offset: 1, limit: 2},
		"count-too-small": {body: `{"state":true,"cid":"42","count":0,"offset":0,"data":[{"fid":"1","cid":"42","n":"a"}]}`, limit: 2},
		"missing-id":      {body: `{"state":true,"cid":"42","count":1,"offset":0,"data":[{"n":"a"}]}`, limit: 2},
		"negative-size":   {body: `{"state":true,"cid":"42","count":1,"offset":0,"data":[{"fid":"1","cid":"42","n":"a","s":"-1"}]}`, limit: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := GetFiles(dirClientForBody(tc.body).NewRequest(), "42", WithApiURL(ApiFileList), WithOffset(tc.offset), WithLimit(tc.limit))
			if !errors.Is(err, ErrUnexpected) {
				t.Fatalf("GetFiles malformed success = %v, want ErrUnexpected", err)
			}
		})
	}
}

func TestGetFilesAcceptsEmptyPagePastEnd(t *testing.T) {
	result, err := GetFiles(dirClientForBody(`{"state":true,"cid":"42","count":1,"offset":0,"data":[]}`).NewRequest(), "42", WithApiURL(ApiFileList), WithOffset(5), WithLimit(2))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Files) != 0 {
		t.Fatalf("unexpected past-end file list: %#v", result)
	}
}

func TestMkdirRejectsMalformedSuccessfulDirectoryID(t *testing.T) {
	for name, body := range map[string]string{
		"missing": `{"state":true}`,
		"zero":    `{"state":true,"cid":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			id, err := dirClientForBody(body).Mkdir("0", "child")
			if id != "" || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("Mkdir malformed success = %q, %v; want empty, ErrUnexpected", id, err)
			}
		})
	}

	id, err := dirClientForBody(`{"state":true,"cid":"123","cname":"child"}`).Mkdir("0", "child")
	if err != nil || id != "123" {
		t.Fatalf("Mkdir valid = %q, %v", id, err)
	}

	id, err = dirClientForBody(`{"state":true,"errno":"","cid":"456","cname":"child"}`).Mkdir("0", "child")
	if err != nil || id != "456" {
		t.Fatalf("Mkdir blank optional errno success = %q, %v", id, err)
	}

	id, err = dirClientForBody(`{"state":false,"errno":"","error":"failed"}`).Mkdir("0", "child")
	if id != "" || !errors.Is(err, ErrUnexpected) {
		t.Fatalf("Mkdir blank optional errno failure = %q, %v; want empty, ErrUnexpected", id, err)
	}
}

func TestDirName2CIDHandlesRootAndRejectsMalformedLookup(t *testing.T) {
	root, err := New().DirName2CID("/")
	if err != nil || root == nil || string(root.CategoryID) != "0" {
		t.Fatalf("DirName2CID root = %#v, %v", root, err)
	}

	for name, body := range map[string]string{
		"missing": `{"state":true}`,
		"zero":    `{"state":true,"id":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := dirClientForBody(body).DirName2CID("movies")
			if resp != nil || !errors.Is(err, ErrNotExist) {
				t.Fatalf("DirName2CID malformed success = %#v, %v; want nil, ErrNotExist", resp, err)
			}
		})
	}

	resp, err := dirClientForBody(`{"state":true,"id":"456"}`).DirName2CID("/movies/")
	if err != nil || resp == nil || string(resp.CategoryID) != "456" {
		t.Fatalf("DirName2CID valid = %#v, %v", resp, err)
	}
}

func dirClientForBody(body string) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	return New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
}
