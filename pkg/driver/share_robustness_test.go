package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func shareSnapClientForBody(body string, calls *int, inspect func(*http.Request)) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls != nil {
			(*calls)++
		}
		if inspect != nil {
			inspect(req)
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

func TestGetShareSnapRejectsInvalidQueryBeforeNetwork(t *testing.T) {
	invalidQueries := map[string]Query{
		"negative-offset": func(query *map[string]string) { (*query)["offset"] = "-1" },
		"zero-limit":      func(query *map[string]string) { (*query)["limit"] = "0" },
		"bad-limit":       func(query *map[string]string) { (*query)["limit"] = "wat" },
		"empty-share":     func(query *map[string]string) { (*query)["share_code"] = " " },
		"empty-dir":       func(query *map[string]string) { (*query)["cid"] = " " },
	}
	for name, query := range invalidQueries {
		t.Run(name, func(t *testing.T) {
			calls := 0
			client := shareSnapClientForBody(`{"state":true,"data":{"count":0,"list":[]}}`, &calls, nil)
			result, err := client.GetShareSnap("share-code", "", "0", query)
			if result != nil || !errors.Is(err, ErrWrongParams) {
				t.Fatalf("GetShareSnap invalid query = %#v, %v; want nil, ErrWrongParams", result, err)
			}
			if calls != 0 {
				t.Fatalf("invalid share query made %d network calls", calls)
			}
		})
	}

	calls := 0
	client := shareSnapClientForBody(`{"state":true,"data":{"count":0,"list":[]}}`, &calls, nil)
	if result, err := client.GetShareSnap(" ", "", "0"); result != nil || !errors.Is(err, ErrWrongParams) || calls != 0 {
		t.Fatalf("blank share code = %#v, %v, calls=%d", result, err, calls)
	}
}

func TestGetShareSnapIgnoresNilQueryAndNormalizesRootDirectory(t *testing.T) {
	client := shareSnapClientForBody(`{"state":true,"data":{"count":1,"list":[{"fid":"f1","n":"a","s":"1","fc":1}]}}`, nil, func(req *http.Request) {
		if got := req.URL.Query().Get("cid"); got != "0" {
			t.Fatalf("share cid = %q, want 0", got)
		}
	})
	result, err := client.GetShareSnap("share-code", "", " ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Data.List) != 1 || result.Data.List[0].FileID != "f1" {
		t.Fatalf("unexpected share result: %#v", result)
	}
}

func TestGetShareSnapRejectsMalformedSuccessfulResponses(t *testing.T) {
	cases := map[string]struct {
		body  string
		query Query
	}{
		"negative-count":  {body: `{"state":true,"data":{"count":-1,"list":[]}}`},
		"count-too-small": {body: `{"state":true,"data":{"count":0,"list":[{"fid":"f1","n":"a","s":"1","fc":1}]}}`},
		"missing-id":      {body: `{"state":true,"data":{"count":1,"list":[{"n":"a","s":"1","fc":1}]}}`},
		"negative-size":   {body: `{"state":true,"data":{"count":1,"list":[{"fid":"f1","n":"a","s":"-1","fc":1}]}}`},
		"bad-category":    {body: `{"state":true,"data":{"count":1,"list":[{"fid":"f1","n":"a","s":"1","fc":2}]}}`},
		"over-limit":      {body: `{"state":true,"data":{"count":2,"list":[{"fid":"f1","n":"a","s":"1","fc":1},{"fid":"f2","n":"b","s":"1","fc":1}]}}`, query: QueryLimit(1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			queries := []Query(nil)
			if tc.query != nil {
				queries = append(queries, tc.query)
			}
			result, err := shareSnapClientForBody(tc.body, nil, nil).GetShareSnap("share-code", "", "0", queries...)
			if result != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("GetShareSnap malformed success = %#v, %v; want nil, ErrUnexpected", result, err)
			}
		})
	}
}
