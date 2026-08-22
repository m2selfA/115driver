package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPrepareMCPShareSnapBatchRejectsInvalidRequestsBeforeClientUse(t *testing.T) {
	for name, args := range map[string]GetShareSnapsArgs{
		"empty":           {},
		"oversized":       {Requests: make([]GetShareSnapsItem, maxMCPShareBatchItems+1)},
		"empty-share":     {Requests: []GetShareSnapsItem{{ShareCode: " "}}},
		"negative-offset": {Requests: []GetShareSnapsItem{{ShareCode: "s", Offset: -1}}},
		"negative-limit":  {Requests: []GetShareSnapsItem{{ShareCode: "s", Limit: -1}}},
		"oversized-page":  {Requests: []GetShareSnapsItem{{ShareCode: "s", Limit: maxMCPShareBatchLimit + 1}}},
		"duplicate-default": {Requests: []GetShareSnapsItem{
			{ShareCode: " s ", ReceiveCode: "pw", DirID: "", Limit: 0},
			{ShareCode: "s", ReceiveCode: "pw", DirID: "0", Limit: defaultMCPShareLimit},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPShareSnapBatch(args); err == nil {
				t.Fatal("expected share batch preflight error")
			}
		})
	}

	tooLarge := GetShareSnapsArgs{Requests: make([]GetShareSnapsItem, 251)}
	for i := range tooLarge.Requests {
		tooLarge.Requests[i] = GetShareSnapsItem{ShareCode: fmt.Sprintf("s-%d", i)}
	}
	if _, err := prepareMCPShareSnapBatch(tooLarge); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("expected aggregate page-budget error, got %v", err)
	}
}

func TestPrepareMCPShareSnapBatchAllowsDifferentPagesForSameShare(t *testing.T) {
	prepared, err := prepareMCPShareSnapBatch(GetShareSnapsArgs{Requests: []GetShareSnapsItem{
		{ShareCode: "same", ReceiveCode: "pw", DirID: "0", Offset: 0, Limit: 10},
		{ShareCode: "same", ReceiveCode: "pw", DirID: "0", Offset: 10, Limit: 10},
		{ShareCode: "same", ReceiveCode: "pw", DirID: "child"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 3 || prepared[2].limit != defaultMCPShareLimit || prepared[2].dirID != "child" {
		t.Fatalf("unexpected prepared share batch: %#v", prepared)
	}
}

func TestGetShareSnapsPreservesOrderContinuesAndRedactsPerItemSecrets(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("get_share_snaps used %s, want GET", req.Method)
		}
		q := req.URL.Query()
		shareCode := q.Get("share_code")
		receiveCode := q.Get("receive_code")
		if shareCode == "bad" {
			return nil, errors.New("network " + receiveCode + " down")
		}
		offset := q.Get("offset")
		count := 2
		name := "file-" + shareCode + "-" + receiveCode
		body := fmt.Sprintf(`{"state":true,"data":{"userinfo":{"user_id":"u","user_name":%q},"shareinfo":{"share_title":%q,"receive_code":%q},"count":%d,"list":[{"fid":%q,"cid":"0","n":%q,"s":"7","fc":1,"pid":"0"}],"share_state":1,"user_appeal":{}}}`, "owner-"+receiveCode, "title-"+receiveCode, receiveCode, count, "f-"+shareCode+"-"+offset, name)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	st := NewShareTools(client)
	result, output, err := st.getShareSnaps(context.Background(), nil, GetShareSnapsArgs{Requests: []GetShareSnapsItem{
		{ShareCode: "first", ReceiveCode: "secret-one", Offset: 0, Limit: 1},
		{ShareCode: "bad", ReceiveCode: "secret-bad", Offset: 0, Limit: 1},
		{ShareCode: "tail", ReceiveCode: "secret-two", Offset: 1, Limit: 2},
	}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("get_share_snaps result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || len(output.Items) != 3 {
		t.Fatalf("unexpected share batch output: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].Data == nil || output.Items[0].Returned != 1 || output.Items[0].NextOffset == nil || *output.Items[0].NextOffset != 1 {
		t.Fatalf("unexpected first share batch item: %#v", output.Items[0])
	}
	if strings.Contains(output.Items[0].Data.Data.ShareInfo.ShareTitle, "secret-one") || strings.Contains(output.Items[0].Data.Data.List[0].FileName, "secret-one") {
		t.Fatalf("first share batch item leaked receive code: %#v", output.Items[0])
	}
	if output.Items[1].Success || strings.Contains(output.Items[1].Error, "secret-bad") || !strings.Contains(output.Items[1].Error, "[REDACTED]") {
		t.Fatalf("failed share batch item was not redacted: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].Data == nil || output.Items[2].NextOffset != nil {
		t.Fatalf("unexpected final share batch item: %#v", output.Items[2])
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, secret := range []string{"secret-one", "secret-bad", "secret-two", "receive_code", "share_code"} {
		if strings.Contains(text, secret) {
			t.Fatalf("share batch text leaked %q: %s", secret, text)
		}
	}
	var decoded GetShareSnapsResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[0].Data.Data.List[0].FileID != output.Items[0].Data.Data.List[0].FileID {
		t.Fatalf("text/typed share batch outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestGetShareSnapsReturnsContinuationForShortNonTerminalPage(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"state":true,"data":{"userinfo":{"user_id":"u","user_name":"owner"},"shareinfo":{"share_title":"title"},"count":10,"list":[{"fid":"f1","cid":"0","n":"one.bin","s":"1","fc":1,"pid":"0"}],"share_state":1,"user_appeal":{}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewShareTools(client).getShareSnaps(context.Background(), nil, GetShareSnapsArgs{Requests: []GetShareSnapsItem{{ShareCode: "share", Offset: 3, Limit: 5}}})
	if err != nil || result == nil || result.IsError || len(output.Items) != 1 {
		t.Fatalf("short share page result=%#v output=%#v err=%v", result, output, err)
	}
	item := output.Items[0]
	if item.Returned != 1 || item.NextOffset == nil || *item.NextOffset != 4 {
		t.Fatalf("short non-terminal share page lost continuation: %#v", item)
	}
}

func TestGetShareSnapsWirePopulatesCredentialFreeStructuredContent(t *testing.T) {
	const receiveCode = "wire-secret"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"state":true,"data":{"userinfo":{"user_id":"u","user_name":"owner"},"shareinfo":{"share_title":%q,"receive_code":%q},"count":1,"list":[{"fid":"f1","cid":"0","n":%q,"s":"1","fc":1,"pid":"0"}],"share_state":1,"user_appeal":{}}}`, "title-"+receiveCode, receiveCode, "file-"+receiveCode)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	server := mcp.NewServer(&mcp.Implementation{Name: "share-batch-test", Version: "1"}, nil)
	NewShareTools(client).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "share-batch-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_share_snaps",
		Arguments: map[string]any{"requests": []any{map[string]any{
			"share_code": "share", "receive_code": receiveCode, "dir_id": "0", "limit": 1,
		}}},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire get_share_snaps result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), receiveCode) || strings.Contains(string(encoded), "receive_code") || strings.Contains(string(encoded), "share_code") {
		t.Fatalf("wire structured share batch leaked credentials: %s", encoded)
	}
	var decoded GetShareSnapsResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 1 || decoded.Succeeded != 1 || len(decoded.Items) != 1 || decoded.Items[0].Data == nil || decoded.Items[0].Data.Data.List[0].FileName != "file-[REDACTED]" {
		t.Fatalf("unexpected wire structured share batch: %#v", decoded)
	}
}
