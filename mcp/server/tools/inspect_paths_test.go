package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInspectPathsRejectsInvalidBatchBeforeClientUse(t *testing.T) {
	dt := NewDirTools(nil)
	for name, args := range map[string]InspectPathsArgs{
		"empty":     {},
		"blank":     {Paths: []string{"/", "   "}},
		"duplicate": {Paths: []string{"/folder/", "folder"}},
		"oversized": {Paths: make([]string, maxMCPInspectPaths+1)},
	} {
		t.Run(name, func(t *testing.T) {
			result, _, err := dt.inspectPaths(context.Background(), nil, args)
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("inspect_paths invalid batch = %#v, %v", result, err)
			}
		})
	}
}

func TestInspectPathsCombinesResolutionAndSafeMetadataAndContinues(t *testing.T) {
	listCalls := 0
	metadataCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("inspect_paths used %s, want GET", req.Method)
		}
		switch req.URL.Path {
		case "/files/getid":
			switch req.URL.Query().Get("path") {
			case "folder":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"42"}`), nil
			case "file.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			case "bad":
				return nil, errors.New("synthetic resolver failure")
			case "after":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"43"}`), nil
			default:
				t.Fatalf("unexpected getid path: %s", req.URL)
			}
		case "/natsort/files.php", "/files":
			listCalls++
			if req.URL.Query().Get("cid") != "0" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("inspect_paths file resolution lost read-only listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"0","count":1,"offset":0,"limit":100,"data":[{"fid":"f1","cid":"0","n":"file.bin","s":"7","pc":"pick1","sha":"ABC","u":"https://thumb.invalid/?token=secret"}]}`), nil
		case "/files/get_info":
			metadataCalls++
			id := req.URL.Query().Get("file_id")
			if id == "43" {
				return nil, errors.New("synthetic metadata failure")
			}
			if id == "42" {
				return mcpResolveJSONResponse(req, `{"state":true,"data":[{"cid":"42","pid":"0","n":"folder","s":"0"}]}`), nil
			}
			if id == "f1" {
				return mcpResolveJSONResponse(req, `{"state":true,"data":[{"fid":"f1","cid":"0","n":"file.bin","s":"7","pc":"pick1","sha":"ABC","u":"https://thumb.invalid/?token=secret"}]}`), nil
			}
			t.Fatalf("unexpected metadata id %q", id)
		default:
			t.Fatalf("unexpected inspect_paths request: %s", req.URL)
		}
		return nil, fmt.Errorf("unreachable")
	})})))

	dt := NewDirTools(client)
	result, output, err := dt.inspectPaths(context.Background(), nil, InspectPathsArgs{Paths: []string{"/", "folder", "file.bin", "bad", "after"}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("inspect_paths result=%#v output=%#v err=%v", result, output, err)
	}
	if listCalls != 1 || metadataCalls != 3 {
		t.Fatalf("inspect_paths calls: list=%d metadata=%d", listCalls, metadataCalls)
	}
	if output.Requested != 5 || output.Succeeded != 3 || output.Failed != 2 || len(output.Items) != 5 {
		t.Fatalf("unexpected inspect_paths summary: %#v", output)
	}
	root := output.Items[0]
	if !root.Success || !root.Resolved || root.FileID != "0" || root.Entry == nil || root.Entry.Name != "/" || root.MetadataComplete {
		t.Fatalf("unexpected root inspection: %#v", root)
	}
	folder := output.Items[1]
	if !folder.Success || !folder.MetadataComplete || folder.Entry == nil || folder.Entry.Name != "folder" || !folder.Entry.IsDirectory {
		t.Fatalf("unexpected folder inspection: %#v", folder)
	}
	file := output.Items[2]
	if !file.Success || file.Entry == nil || file.Entry.Size != 7 || file.Entry.PickCode != "pick1" {
		t.Fatalf("unexpected file inspection: %#v", file)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "thumb.invalid") || strings.Contains(string(encoded), "token=secret") {
		t.Fatalf("typed inspection leaked thumbnail URL: %s", encoded)
	}
	if output.Items[3].Resolved || output.Items[3].Success || !strings.Contains(output.Items[3].Error, "synthetic resolver failure") {
		t.Fatalf("unexpected resolver failure: %#v", output.Items[3])
	}
	if !output.Items[4].Resolved || output.Items[4].Success || !strings.Contains(output.Items[4].Error, "synthetic metadata failure") {
		t.Fatalf("unexpected metadata failure: %#v", output.Items[4])
	}
}

func TestInspectPathsReusesParentDirectoryPagesWithinOneBatch(t *testing.T) {
	listCalls := 0
	metadataCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
		case "/natsort/files.php", "/files":
			listCalls++
			body := `{"state":true,"cid":"0","count":2,"offset":0,"limit":100,"data":[{"fid":"f1","cid":"0","n":"a.bin","s":"1"},{"fid":"f2","cid":"0","n":"b.bin","s":"2"}]}`
			return mcpResolveJSONResponse(req, body), nil
		case "/files/get_info":
			metadataCalls++
			id := req.URL.Query().Get("file_id")
			name := "a.bin"
			size := "1"
			if id == "f2" {
				name, size = "b.bin", "2"
			}
			return mcpResolveJSONResponse(req, fmt.Sprintf(`{"state":true,"data":[{"fid":%q,"cid":"0","n":%q,"s":%q}]}`, id, name, size)), nil
		default:
			t.Fatalf("unexpected inspect_paths shared-parent request: %s", req.URL)
			return nil, errors.New("unreachable")
		}
	})})))

	result, output, err := NewDirTools(client).inspectPaths(context.Background(), nil, InspectPathsArgs{Paths: []string{"a.bin", "b.bin"}})
	if err != nil || result == nil || result.IsError || output.Succeeded != 2 {
		t.Fatalf("inspect_paths shared-parent result=%#v output=%#v err=%v", result, output, err)
	}
	if listCalls != 1 || metadataCalls != 2 {
		t.Fatalf("inspect_paths shared-parent calls list=%d metadata=%d, want 1/2", listCalls, metadataCalls)
	}
	if output.Items[0].Entry == nil || output.Items[0].Entry.FileID != "f1" || output.Items[1].Entry == nil || output.Items[1].Entry.FileID != "f2" {
		t.Fatalf("unexpected inspect_paths shared-parent output: %#v", output.Items)
	}
}

func TestInspectPathsWirePopulatesStructuredContent(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/files/getid" {
			return mcpResolveJSONResponse(req, `{"state":true,"id":"42"}`), nil
		}
		if req.URL.Path == "/files/get_info" {
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"cid":"42","pid":"0","n":"folder","s":"0"}]}`), nil
		}
		t.Fatalf("unexpected wire inspect request: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	server := mcp.NewServer(&mcp.Implementation{Name: "inspect-paths-test", Version: "1"}, nil)
	NewDirTools(client).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "inspect-paths-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "inspect_paths", Arguments: map[string]any{"paths": []any{"folder"}}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire inspect_paths result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded InspectPathsResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Succeeded != 1 || len(decoded.Items) != 1 || decoded.Items[0].Entry == nil || decoded.Items[0].Entry.Name != "folder" {
		t.Fatalf("unexpected wire structured inspection: %#v", decoded)
	}
}
