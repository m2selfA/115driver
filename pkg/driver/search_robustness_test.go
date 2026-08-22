package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func searchClientForBody(body string, calls *int) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls != nil {
			(*calls)++
		}
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

func TestSearchRejectsInvalidOptionsBeforeNetwork(t *testing.T) {
	cases := map[string]*SearchOption{
		"negative-offset": {Offset: -1},
		"negative-limit":  {Limit: -1},
		"invalid-type":    {Type: 7},
		"count-folders":   {CountFolders: 2},
		"invalid-asc":     {Asc: 2},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := searchClientForBody(`{"state":true,"count":0,"offset":0,"data":[]}`, &calls)
			result, err := client.Search(opts)
			if result != nil || !errors.Is(err, ErrWrongParams) {
				t.Fatalf("Search invalid options = %#v, %v; want nil, ErrWrongParams", result, err)
			}
			if calls != 0 {
				t.Fatalf("invalid search options made %d network calls", calls)
			}
		})
	}
}

func TestSearchRejectsMalformedSuccessfulResponses(t *testing.T) {
	cases := map[string]struct {
		opts *SearchOption
		body string
	}{
		"offset-mismatch": {opts: &SearchOption{Offset: 10, Limit: 2}, body: `{"state":true,"count":11,"offset":0,"data":[{"fid":"f1","cid":"0","n":"a","s":"1"}]}`},
		"over-limit":      {opts: &SearchOption{Limit: 1}, body: `{"state":true,"count":2,"offset":0,"data":[{"fid":"f1","cid":"0","n":"a","s":"1"},{"fid":"f2","cid":"0","n":"b","s":"1"}]}`},
		"count-too-small": {opts: &SearchOption{Limit: 2}, body: `{"state":true,"count":0,"offset":0,"data":[{"fid":"f1","cid":"0","n":"a","s":"1"}]}`},
		"missing-id":      {opts: &SearchOption{Limit: 2}, body: `{"state":true,"count":1,"offset":0,"data":[{"n":"a","s":"1"}]}`},
		"negative-size":   {opts: &SearchOption{Limit: 2}, body: `{"state":true,"count":1,"offset":0,"data":[{"fid":"f1","cid":"0","n":"a","s":"-1"}]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := searchClientForBody(tc.body, nil).Search(tc.opts)
			if result != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("Search malformed success = %#v, %v; want nil, ErrUnexpected", result, err)
			}
		})
	}
}

func TestSearchAcceptsSelfConsistentSuccess(t *testing.T) {
	result, err := searchClientForBody(`{"state":true,"count":1,"offset":0,"order":"file_name","is_asc":0,"data":[{"fid":"f1","cid":"0","n":"a","s":"1"}]}`, nil).Search(&SearchOption{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Files) != 1 || result.Files[0].FileID != "f1" || result.Count != 1 {
		t.Fatalf("unexpected Search result: %#v", result)
	}
}
