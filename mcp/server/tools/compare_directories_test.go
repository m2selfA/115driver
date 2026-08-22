package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeMCPCompareDirectoriesArgsBoundsAndDefaults(t *testing.T) {
	left, right, maxNodes, err := normalizeMCPCompareDirectoriesArgs(CompareDirectoriesArgs{LeftPath: "/left", RightPath: "/right"})
	if err != nil || left != "/left" || right != "/right" || maxNodes != defaultMCPListTreeMaxNodes {
		t.Fatalf("compare defaults = left=%q right=%q maxNodes=%d err=%v", left, right, maxNodes, err)
	}
	for name, args := range map[string]CompareDirectoriesArgs{
		"blank-left":     {RightPath: "/right"},
		"blank-right":    {LeftPath: "/left"},
		"same-logical":   {LeftPath: "/same/", RightPath: "same"},
		"negative-depth": {LeftPath: "/left", RightPath: "/right", MaxDepth: -1},
		"negative-nodes": {LeftPath: "/left", RightPath: "/right", MaxNodes: -1},
		"one-node":       {LeftPath: "/left", RightPath: "/right", MaxNodes: 1},
		"too-many-nodes": {LeftPath: "/left", RightPath: "/right", MaxNodes: maxMCPListTreeMaxNodes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := normalizeMCPCompareDirectoriesArgs(args); err == nil {
				t.Fatal("expected compare_directories preflight failure")
			}
		})
	}
}

func TestCompareMCPDirectoriesClassifiesStableDiffs(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"left": "l", "right": "r"},
		treePages: map[string][]driver.File{
			"l": {
				{FileID: "la", ParentID: "l", Name: "a.bin", Size: 1, Sha1: "same"},
				{FileID: "ll", ParentID: "l", Name: "left-only.bin", Size: 2},
				{FileID: "lt", ParentID: "l", Name: "type", Size: 3},
				{FileID: "lm", ParentID: "l", Name: "meta.bin", Size: 4, Sha1: "old"},
				{FileID: "lu", ParentID: "l", Name: "unchanged.bin", Size: 5, Sha1: "stable", Star: true},
			},
			"r": {
				{FileID: "ra", ParentID: "r", Name: "a.bin", Size: 1, Sha1: "same"},
				{FileID: "rr", ParentID: "r", Name: "right-only.bin", Size: 6},
				{FileID: "rt", ParentID: "r", Name: "type", IsDirectory: true},
				{FileID: "rm", ParentID: "r", Name: "meta.bin", Size: 7, Sha1: "new"},
				{FileID: "ru", ParentID: "r", Name: "unchanged.bin", Size: 5, Sha1: "stable", Star: true},
			},
		},
	}

	result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxNodes: 20, IncludeUnchanged: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || !result.AbsenceComparisonComplete || result.BudgetExhausted || result.NodesVisited != 10 {
		t.Fatalf("unexpected compare completeness: %#v", result)
	}
	if result.OnlyLeftCount != 1 || result.OnlyRightCount != 1 || result.TypeChangedCount != 1 || result.MetadataChangedCount != 1 || result.UnchangedCount != 2 || result.UnverifiedLeftCount != 0 || result.UnverifiedRightCount != 0 {
		t.Fatalf("unexpected compare counts: %#v", result)
	}
	if result.OnlyLeft[0].RelativePath != "left-only.bin" || result.OnlyRight[0].RelativePath != "right-only.bin" || result.TypeChanged[0].RelativePath != "type" || result.MetadataChanged[0].RelativePath != "meta.bin" {
		t.Fatalf("unexpected compare classes: %#v", result)
	}
	if got := result.MetadataChanged[0].ChangedFields; len(got) != 2 || got[0] != "size" || got[1] != "sha1" {
		t.Fatalf("metadata changed fields = %v", got)
	}
	if len(result.Unchanged) != 2 || result.Unchanged[0].RelativePath != "a.bin" || result.Unchanged[1].RelativePath != "unchanged.bin" {
		t.Fatalf("unchanged output is not deterministic: %#v", result.Unchanged)
	}
}

func TestCompareMCPDirectoriesSuppressesFalseAbsenceWhenNodeLimited(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"left": "l", "right": "r"},
		treePages: map[string][]driver.File{
			"l": {
				{FileID: "l1", Name: "left-visible.bin"},
				{FileID: "l2", Name: "left-hidden.bin"},
			},
			"r": {
				{FileID: "r1", Name: "right-visible.bin"},
				{FileID: "r2", Name: "right-hidden.bin"},
			},
		},
	}

	result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.AbsenceComparisonComplete || !result.BudgetExhausted || result.OnlyLeftCount != 0 || result.OnlyRightCount != 0 {
		t.Fatalf("node-limited compare made unsafe absence claims: %#v", result)
	}
	if result.UnverifiedLeftCount != 1 || result.UnverifiedRightCount != 1 {
		t.Fatalf("node-limited unmatched entries were not preserved as unverified: %#v", result)
	}
}

func TestCompareMCPDirectoriesDepthLimitStillProvesVisibleAbsence(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"left": "l", "right": "r"},
		treePages: map[string][]driver.File{
			"l": {{FileID: "ld", Name: "left-dir", IsDirectory: true}},
			"r": {{FileID: "rd", Name: "right-dir", IsDirectory: true}},
		},
	}

	result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxDepth: 1, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || !result.Left.DepthLimited || !result.Right.DepthLimited || !result.AbsenceComparisonComplete {
		t.Fatalf("depth-limited visible scope semantics are wrong: %#v", result)
	}
	if result.OnlyLeftCount != 1 || result.OnlyRightCount != 1 || result.UnverifiedLeftCount != 0 || result.UnverifiedRightCount != 0 {
		t.Fatalf("depth-limited visible absences were not classified: %#v", result)
	}
}

func TestCompareMCPDirectoriesReusesUnusedBudgetAcrossSides(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"left": "l", "right": "r"},
		treePages: map[string][]driver.File{
			"l": {},
			"r": {
				{FileID: "r1", Name: "one.bin"},
				{FileID: "r2", Name: "two.bin"},
				{FileID: "r3", Name: "three.bin"},
			},
		},
	}

	result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxNodes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Left.NodeBudget != 0 || result.Right.NodeBudget != 3 || result.Right.NodesVisited != 3 || result.Right.NodeLimited || result.OnlyRightCount != 3 {
		t.Fatalf("unused left budget was not transferred to the right: %#v", result)
	}
	if result.Left.NodeBudget+result.Right.NodeBudget > result.MaxNodes || result.NodesVisited > result.MaxNodes {
		t.Fatalf("final compare budget exceeds aggregate max_nodes: %#v", result)
	}
}

func TestCompareMCPDirectorySnapshotsRejectsDuplicateRelativePaths(t *testing.T) {
	left := ListTreeItemResult{Success: true, Complete: true, Entries: []MCPRemoteTreeEntry{
		{FileID: "a", RelativePath: "dup.bin"},
		{FileID: "b", RelativePath: "dup.bin"},
	}}
	right := ListTreeItemResult{Success: true, Complete: true}
	if _, err := compareMCPDirectorySnapshots(left, right, 10, 10, 0, 20, false); err == nil {
		t.Fatal("duplicate relative paths were not rejected")
	}
}

func TestCompareMCPDirectoriesCachedRetryDoesNotRepeatRemotePageReads(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"left": "l", "right": "r"},
		treePages: map[string][]driver.File{
			"l": {
				{FileID: "l1", Name: "one.bin"},
				{FileID: "l2", Name: "two.bin"},
				{FileID: "l3", Name: "three.bin"},
			},
			"r": {},
		},
	}
	result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxNodes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Left.NodesVisited != 3 || result.Left.NodeBudget != 3 || result.Right.NodeBudget != 0 || result.Left.NodeLimited || result.OnlyLeftCount != 3 {
		t.Fatalf("left budget was not expanded from unused right capacity: %#v", result)
	}
	if result.Left.NodeBudget+result.Right.NodeBudget > result.MaxNodes || result.NodesVisited > result.MaxNodes {
		t.Fatalf("cached retry final budget exceeds aggregate max_nodes: %#v", result)
	}
	if got := client.pageCalls["tree:l"]; got != 1 {
		t.Fatalf("cached left retry issued %d underlying tree page reads, want 1", got)
	}
}

func TestCompareMCPDirectoriesFinalBudgetMetadataNeverExceedsAggregateLimit(t *testing.T) {
	cases := []struct {
		name     string
		maxNodes int
		left     []driver.File
		right    []driver.File
	}{
		{name: "left-finishes-early", maxNodes: 6, left: []driver.File{{FileID: "l1", Name: "l1"}}, right: []driver.File{{FileID: "r1", Name: "r1"}, {FileID: "r2", Name: "r2"}, {FileID: "r3", Name: "r3"}, {FileID: "r4", Name: "r4"}, {FileID: "r5", Name: "r5"}}},
		{name: "right-finishes-early", maxNodes: 6, left: []driver.File{{FileID: "l1", Name: "l1"}, {FileID: "l2", Name: "l2"}, {FileID: "l3", Name: "l3"}, {FileID: "l4", Name: "l4"}, {FileID: "l5", Name: "l5"}}, right: []driver.File{{FileID: "r1", Name: "r1"}}},
		{name: "both-hit-caps", maxNodes: 6, left: []driver.File{{FileID: "l1", Name: "l1"}, {FileID: "l2", Name: "l2"}, {FileID: "l3", Name: "l3"}, {FileID: "l4", Name: "l4"}}, right: []driver.File{{FileID: "r1", Name: "r1"}, {FileID: "r2", Name: "r2"}, {FileID: "r3", Name: "r3"}, {FileID: "r4", Name: "r4"}}},
		{name: "both-finish-early", maxNodes: 10, left: []driver.File{{FileID: "l1", Name: "l1"}, {FileID: "l2", Name: "l2"}}, right: []driver.File{{FileID: "r1", Name: "r1"}, {FileID: "r2", Name: "r2"}, {FileID: "r3", Name: "r3"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &usageTestClient{
				dirIDs:    map[string]string{"left": "l", "right": "r"},
				treePages: map[string][]driver.File{"l": tc.left, "r": tc.right},
			}
			result, err := compareMCPDirectories(context.Background(), client, CompareDirectoriesArgs{LeftPath: "left", RightPath: "right", MaxNodes: tc.maxNodes})
			if err != nil {
				t.Fatal(err)
			}
			if result.NodesVisited > tc.maxNodes {
				t.Fatalf("NodesVisited=%d exceeds max_nodes=%d: %#v", result.NodesVisited, tc.maxNodes, result)
			}
			if result.Left.NodeBudget+result.Right.NodeBudget > tc.maxNodes {
				t.Fatalf("final side budgets %d+%d exceed max_nodes=%d: %#v", result.Left.NodeBudget, result.Right.NodeBudget, tc.maxNodes, result)
			}
			if result.Left.NodesVisited > result.Left.NodeBudget || result.Right.NodesVisited > result.Right.NodeBudget {
				t.Fatalf("side visited count exceeds retained budget: %#v", result)
			}
		})
	}
}

func TestCompareDirectoriesCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := CompareDirectoriesResult{
		Left:           MCPDirectoryCompareSide{Path: "/a", Success: true, Complete: true},
		Right:          MCPDirectoryCompareSide{Path: "/b", Success: true, Complete: true},
		Complete:       true,
		OnlyLeft:       []MCPRemoteTreeEntry{{FileID: "f1", Name: "a.bin", RelativePath: "a.bin"}},
		OnlyLeftCount:  1,
		UnchangedCount: 2,
	}
	result, output, err := compareDirectoriesCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("compare call result=%#v output=%#v err=%v", result, output, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected compare content: %#v", result.Content[0])
	}
	var decoded CompareDirectoriesResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OnlyLeftCount != output.OnlyLeftCount || decoded.UnchangedCount != output.UnchangedCount || decoded.Left.Path != output.Left.Path {
		t.Fatalf("text/typed compare outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}
