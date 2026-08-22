package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeMCPListTreeArgsBoundsAndDefaults(t *testing.T) {
	paths, maxNodes, err := normalizeMCPListTreeArgs(ListTreeArgs{Paths: []string{"/a"}})
	if err != nil || len(paths) != 1 || maxNodes != defaultMCPListTreeMaxNodes {
		t.Fatalf("list_tree defaults = paths=%v maxNodes=%d err=%v", paths, maxNodes, err)
	}
	for name, args := range map[string]ListTreeArgs{
		"empty":          {},
		"negative-depth": {Paths: []string{"/a"}, MaxDepth: -1},
		"negative-nodes": {Paths: []string{"/a"}, MaxNodes: -1},
		"too-many-nodes": {Paths: []string{"/a"}, MaxNodes: maxMCPListTreeMaxNodes + 1},
		"duplicate":      {Paths: []string{"/a/", "a"}},
		"too-many-paths": {Paths: make([]string, maxMCPListTreePaths+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := normalizeMCPListTreeArgs(args); err == nil {
				t.Fatal("expected list_tree preflight failure")
			}
		})
	}
}

func TestListMCPRemoteTreeMatchesRecursiveLSOrderDepthAndContinues(t *testing.T) {
	client := &usageTestClient{
		dirIDs:  map[string]string{"root": "d1", "after": "d3"},
		dirErrs: map[string]error{"bad": errors.New("synthetic tree resolver failure")},
		treePages: map[string][]driver.File{
			"d1": {
				{FileID: "fa", ParentID: "d1", Name: "a.bin", Size: 1},
				{FileID: "d2", ParentID: "d1", Name: "sub", IsDirectory: true},
			},
			"d2": {{FileID: "fb", ParentID: "d2", Name: "b.bin", Size: 2}},
			"d3": {},
		},
	}
	response, err := listMCPRemoteTree(context.Background(), client, ListTreeArgs{Paths: []string{"root", "bad", "after"}, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if response.Requested != 3 || response.Succeeded != 2 || response.Failed != 1 || response.NodesVisited != 3 || response.BudgetExhausted {
		t.Fatalf("unexpected list_tree result: %#v", response)
	}
	first := response.Items[0]
	if !first.Success || !first.Complete || len(first.Entries) != 3 {
		t.Fatalf("unexpected first tree: %#v", first)
	}
	if first.Entries[0].RelativePath != "a.bin" || first.Entries[0].Depth != 1 || first.Entries[1].RelativePath != "sub" || first.Entries[1].Depth != 1 || first.Entries[2].RelativePath != "sub/b.bin" || first.Entries[2].Path != "root/sub/b.bin" || first.Entries[2].Depth != 2 {
		t.Fatalf("recursive listing order/path metadata mismatch: %#v", first.Entries)
	}
	if response.Items[1].Success || response.Items[1].Error == "" {
		t.Fatalf("failed tree path lost error: %#v", response.Items[1])
	}
	if !response.Items[2].Success || !response.Items[2].Complete || len(response.Items[2].Entries) != 0 {
		t.Fatalf("list_tree did not continue after failure: %#v", response.Items[2])
	}
}

func TestListMCPRemoteTreeDepthAndNodeLimitsAreExplicitPartialSuccess(t *testing.T) {
	depthClient := &usageTestClient{
		dirIDs: map[string]string{"root": "d1"},
		treePages: map[string][]driver.File{
			"d1": {{FileID: "d2", Name: "sub", IsDirectory: true}},
			"d2": {{FileID: "f1", Name: "deep.bin", Size: 1}},
		},
	}
	depth, err := listMCPRemoteTree(context.Background(), depthClient, ListTreeArgs{Paths: []string{"root"}, MaxDepth: 1, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if depth.Failed != 0 || len(depth.Items) != 1 || !depth.Items[0].Success || depth.Items[0].Complete || !depth.Items[0].DepthLimited || depth.Items[0].NodeLimited || len(depth.Items[0].Entries) != 1 {
		t.Fatalf("unexpected depth-limited tree: %#v", depth)
	}

	nodeClient := &usageTestClient{
		dirIDs: map[string]string{"root": "d1", "after": "d2"},
		treePages: map[string][]driver.File{
			"d1": {
				{FileID: "f1", Name: "one.bin"},
				{FileID: "f2", Name: "two.bin"},
				{FileID: "f3", Name: "three.bin"},
			},
			"d2": {},
		},
	}
	node, err := listMCPRemoteTree(context.Background(), nodeClient, ListTreeArgs{Paths: []string{"root", "after"}, MaxNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if node.Succeeded != 1 || node.Failed != 1 || node.NodesVisited != 2 || !node.BudgetExhausted || !node.Items[0].Success || node.Items[0].Complete || !node.Items[0].NodeLimited || len(node.Items[0].Entries) != 2 {
		t.Fatalf("unexpected node-limited tree: %#v", node)
	}
	if nodeClient.dirCalls["after"] != 0 {
		t.Fatalf("budget-exhausted later root reached resolver %d times", nodeClient.dirCalls["after"])
	}
}

func TestListTreeCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := ListTreeResult{
		Requested: 1,
		Succeeded: 1,
		MaxNodes:  10,
		Items:     []ListTreeItemResult{{Index: 0, Path: "/", Success: true, Complete: true, Entries: []MCPRemoteTreeEntry{{FileID: "f1", Name: "a.bin", RelativePath: "a.bin", Path: "/a.bin", Depth: 1}}}},
	}
	result, output, err := listTreeCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("list_tree call result=%#v output=%#v err=%v", result, output, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected list_tree content: %#v", result.Content[0])
	}
	var decoded ListTreeResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[0].Entries[0].Path != output.Items[0].Entries[0].Path {
		t.Fatalf("text/typed tree outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}
