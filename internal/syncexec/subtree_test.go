package syncexec

import (
	"strings"
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestExpectedSubtreeUsesSideSpecificReplacementKindAndCoveredDescendants(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{
			RelativePath: "dir", Action: "replace-remote", Kind: "file", ReplacesKind: "directory",
			LocalPresent: true, RemotePresent: true, RemoteID: "r-dir", RemoteModTimeUnixNano: 10,
		},
		{
			RelativePath: "dir/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-replace-remote:dir",
			RemotePresent: true, RemoteID: "r-child", RemoteSize: 4, RemoteSHA1: "ABC", RemoteModTimeUnixNano: 11,
		},
	}}
	nodes, err := ExpectedSubtree(plan, "dir", SubtreeRemote)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Kind != "directory" || nodes[0].ObjectID != "r-dir" || nodes[1].RelativePath != "dir/child.bin" || nodes[1].SHA1 != "ABC" {
		t.Fatalf("unexpected reviewed remote subtree: %#v", nodes)
	}
}

func TestExpectedSubtreeRequiresContentSnapshotForFiles(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{{
		RelativePath: "dir/file.bin", Action: "skip", Kind: "file", LocalPresent: true, LocalSize: 4,
	}}}
	if _, err := ExpectedSubtree(plan, "dir/file.bin", SubtreeLocal); err == nil || !strings.Contains(err.Error(), "content snapshot") {
		t.Fatalf("missing local content snapshot error = %v", err)
	}
}

func TestCompareSubtreeRejectsNewNodesIdentityAndContentChanges(t *testing.T) {
	expected := []SubtreeNode{
		{RelativePath: "dir", Kind: "directory", ObjectID: "d", ModTimeUnixNano: 1},
		{RelativePath: "dir/a.bin", Kind: "file", Size: 4, ObjectID: "a", SHA1: "AAAA", ModTimeUnixNano: 2},
	}
	if err := CompareSubtree(expected, append([]SubtreeNode(nil), expected...)); err != nil {
		t.Fatalf("identical subtree rejected: %v", err)
	}
	newNode := append(append([]SubtreeNode(nil), expected...), SubtreeNode{RelativePath: "dir/new.bin", Kind: "file", Size: 1, ObjectID: "n", SHA1: "N"})
	if err := CompareSubtree(expected, newNode); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("new subtree node was accepted: %v", err)
	}
	changedID := append([]SubtreeNode(nil), expected...)
	changedID[1].ObjectID = "different"
	if err := CompareSubtree(expected, changedID); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("changed remote identity was accepted: %v", err)
	}
	changedContent := append([]SubtreeNode(nil), expected...)
	changedContent[1].SHA1 = "BBBB"
	if err := CompareSubtree(expected, changedContent); err == nil || !strings.Contains(err.Error(), "content") {
		t.Fatalf("same-size content rewrite was accepted: %v", err)
	}
}
