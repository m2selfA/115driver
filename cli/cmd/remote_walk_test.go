package cmd

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeRemoteTreeListClient struct {
	lists map[string][]driver.File
	calls []string
}

func (client *fakeRemoteTreeListClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	client.calls = append(client.calls, dirID)
	entries, ok := client.lists[dirID]
	if !ok {
		return nil, fmt.Errorf("unexpected directory %s", dirID)
	}
	cloned := append([]driver.File(nil), entries...)
	return &cloned, nil
}

func testRemoteWalkTree() *fakeRemoteTreeListClient {
	return &fakeRemoteTreeListClient{lists: map[string][]driver.File{
		"root": {
			{FileID: "f-a", Name: "a.bin", Size: 1},
			{FileID: "d-one", Name: "one", IsDirectory: true},
		},
		"d-one": {
			{FileID: "f-b", Name: "b.bin", Size: 2},
			{FileID: "d-two", Name: "two", IsDirectory: true},
		},
		"d-two": {
			{FileID: "f-c", Name: "c.bin", Size: 3},
		},
	}}
}

func TestCollectRecursiveLSUsesGlobalOffsetLimitAndPaths(t *testing.T) {
	client := testRemoteWalkTree()
	files, hasMore, depthLimited, err := collectRecursiveLS(client, "root", "/root", 1, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || depthLimited {
		t.Fatalf("unexpected pagination/depth state: hasMore=%v depthLimited=%v", hasMore, depthLimited)
	}
	got := []string{files[0].RelativePath, files[1].RelativePath}
	want := []string{"one", "one/b.bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected recursive page: got %v want %v", got, want)
	}
	if files[0].RemotePath != "/root/one" || files[0].Depth != 1 || files[1].Depth != 2 {
		t.Fatalf("recursive path metadata was not preserved: %#v", files)
	}
}

func TestCollectRecursiveLSMaxDepthStopsDescending(t *testing.T) {
	client := testRemoteWalkTree()
	files, hasMore, depthLimited, err := collectRecursiveLS(client, "root", "/root", 0, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || !depthLimited {
		t.Fatalf("unexpected bounded traversal state: hasMore=%v depthLimited=%v", hasMore, depthLimited)
	}
	got := make([]string, 0, len(files))
	for _, file := range files {
		got = append(got, file.RelativePath)
	}
	if want := []string{"a.bin", "one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("max-depth traversal mismatch: got %v want %v", got, want)
	}
	if !reflect.DeepEqual(client.calls, []string{"root"}) {
		t.Fatalf("max-depth unexpectedly descended: %v", client.calls)
	}
}

func TestCalculateDirectoryUsageUnlimitedAndDepthLimited(t *testing.T) {
	full, err := calculateDirectoryUsage(testRemoteWalkTree(), "root", "/root", 0)
	if err != nil {
		t.Fatal(err)
	}
	if full.Size != 6 || full.Files != 3 || full.Directories != 2 || !full.Complete {
		t.Fatalf("unexpected full usage: %#v", full)
	}

	bounded, err := calculateDirectoryUsage(testRemoteWalkTree(), "root", "/root", 1)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Size != 1 || bounded.Files != 1 || bounded.Directories != 1 || bounded.Complete {
		t.Fatalf("unexpected depth-limited usage: %#v", bounded)
	}
}

func TestWalkRemoteTreeRejectsDirectoryCycles(t *testing.T) {
	client := &fakeRemoteTreeListClient{lists: map[string][]driver.File{
		"root": {{FileID: "root", Name: "cycle", IsDirectory: true}},
	}}
	_, err := walkRemoteTree(client, "root", "/root", 0, func(remoteWalkEntry) (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("expected repeated directory ID to be rejected")
	}
}
