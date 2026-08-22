package driver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithAscSetsAscWithoutChangingShowDir(t *testing.T) {
	o := DefaultGetFileOptions()
	WithAsc(false)(o)
	if got := o.GetAsc(); got != "0" {
		t.Fatalf("WithAsc(false) asc = %q, want 0", got)
	}
	if got := o.GetshowDir(); got != "1" {
		t.Fatalf("WithAsc(false) show_dir = %q, want unchanged 1", got)
	}

	WithAsc(true)(o)
	if got := o.GetAsc(); got != "1" {
		t.Fatalf("WithAsc(true) asc = %q, want 1", got)
	}
}

func TestListOptionsControlRecordOpenTime(t *testing.T) {
	tests := []struct {
		name string
		call func(*Pan115Client, string) error
		want string
	}{
		{
			name: "page-default-records-open-time",
			call: func(client *Pan115Client, endpoint string) error {
				_, err := client.ListPage("0", 0, 1, WithApiURLs(endpoint))
				return err
			},
			want: "1",
		},
		{
			name: "page-can-disable-open-time-recording",
			call: func(client *Pan115Client, endpoint string) error {
				_, err := client.ListPage("0", 0, 1, WithApiURLs(endpoint), WithRecordOpenTime(false))
				return err
			},
			want: "0",
		},
		{
			name: "full-list-can-disable-open-time-recording",
			call: func(client *Pan115Client, endpoint string) error {
				_, err := client.ListWithLimit("0", 1, WithApiURLs(endpoint), WithRecordOpenTime(false))
				return err
			},
			want: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("record_open_time")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"state":true,"cid":"0","count":0,"offset":0,"limit":1,"data":[]}`))
			}))
			defer server.Close()

			if err := tt.call(New(), server.URL); err != nil {
				t.Fatalf("list request failed: %v", err)
			}
			if got != tt.want {
				t.Fatalf("record_open_time = %q, want %q", got, tt.want)
			}
		})
	}
}
