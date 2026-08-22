package driver

import (
	"errors"
	"net/http"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestCollectOfflineTaskHashesRequiresOneSuccessfulResultPerInput(t *testing.T) {
	valid := offlineAddURLWireResponse{Result: []offlineTaskWireResponse{
		{OfflineTaskResponse: OfflineTaskResponse{InfoHash: " hash-a ", Url: "a"}, State: boolPtr(true)},
		{OfflineTaskResponse: OfflineTaskResponse{InfoHash: "hash-b", Url: "b"}},
	}}
	hashes, err := collectOfflineTaskHashes(valid, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 || hashes[0] != "hash-a" || hashes[1] != "hash-b" {
		t.Fatalf("offline hashes = %#v", hashes)
	}

	for name, tc := range map[string]struct {
		resp     offlineAddURLWireResponse
		expected int
		want     error
	}{
		"too-few":          {resp: offlineAddURLWireResponse{Result: []offlineTaskWireResponse{{OfflineTaskResponse: OfflineTaskResponse{InfoHash: "one"}}}}, expected: 2, want: ErrUnexpected},
		"too-many":         {resp: offlineAddURLWireResponse{Result: []offlineTaskWireResponse{{OfflineTaskResponse: OfflineTaskResponse{InfoHash: "one"}}, {OfflineTaskResponse: OfflineTaskResponse{InfoHash: "two"}}}}, expected: 1, want: ErrUnexpected},
		"explicit-failure": {resp: offlineAddURLWireResponse{Result: []offlineTaskWireResponse{{State: boolPtr(false), ErrorMsg: "bad link"}}}, expected: 1, want: ErrUnexpected},
		"existing-task":    {resp: offlineAddURLWireResponse{Result: []offlineTaskWireResponse{{State: boolPtr(false), ErrCode: 10008}}}, expected: 1, want: ErrOfflineTaskExisted},
		"missing-hash":     {resp: offlineAddURLWireResponse{Result: []offlineTaskWireResponse{{State: boolPtr(true)}}}, expected: 1, want: ErrUnexpected},
	} {
		t.Run(name, func(t *testing.T) {
			hashes, err := collectOfflineTaskHashes(tc.resp, tc.expected)
			if hashes != nil || !errors.Is(err, tc.want) {
				t.Fatalf("collectOfflineTaskHashes = %#v, %v; want nil, %v", hashes, err, tc.want)
			}
		})
	}
}

func TestValidateOfflineTaskPageRejectsMalformedSuccessfulMetadata(t *testing.T) {
	for name, tc := range map[string]struct {
		result    OfflineTaskResp
		requested int64
	}{
		"negative-total":      {result: OfflineTaskResp{Total: -1}, requested: 1},
		"negative-page-count": {result: OfflineTaskResp{PageCount: -1}, requested: 1},
		"wrong-page":          {result: OfflineTaskResp{Page: 2, PageCount: 2}, requested: 1},
		"page-beyond-count":   {result: OfflineTaskResp{Page: 3, PageCount: 2}, requested: 3},
		"null-task":           {result: OfflineTaskResp{Tasks: []*OfflineTask{nil}}, requested: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOfflineTaskPage(&tc.result, tc.requested); !errors.Is(err, ErrUnexpected) {
				t.Fatalf("validateOfflineTaskPage = %v, want ErrUnexpected", err)
			}
		})
	}
	if err := validateOfflineTaskPage(&OfflineTaskResp{Page: 0, PageCount: 0}, 1); err != nil {
		t.Fatalf("legacy omitted page metadata should remain valid: %v", err)
	}
}

func TestOfflinePublicInputValidationDoesNotReachNetwork(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid offline input unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))

	if _, err := client.ListOfflineTask(0); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("ListOfflineTask(0) = %v, want ErrWrongParams", err)
	}
	if _, err := client.AddOfflineTaskURIs([]string{""}, "0", nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("AddOfflineTaskURIs(empty) = %v, want ErrWrongParams", err)
	}
	if err := client.DeleteOfflineTasks(nil, false); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("DeleteOfflineTasks(nil) = %v, want ErrWrongParams", err)
	}
	if err := client.DeleteOfflineTasks([]string{"  "}, true); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("DeleteOfflineTasks(blank) = %v, want ErrWrongParams", err)
	}
}
