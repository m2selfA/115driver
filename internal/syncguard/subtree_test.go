package syncguard

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type subtreeTestRemoteClient struct {
	lists map[string][]driver.File
}

func (client *subtreeTestRemoteClient) List(dirID string, opts ...driver.ListOption) (*[]driver.File, error) {
	items := append([]driver.File(nil), client.lists[dirID]...)
	return &items, nil
}

type subtreePagedTestClient struct {
	listCalls int
	pageCalls int
}

func (client *subtreePagedTestClient) List(dirID string, opts ...driver.ListOption) (*[]driver.File, error) {
	client.listCalls++
	items := []driver.File{{FileID: "legacy", Name: "legacy.bin", Size: 1, Sha1: "LEGACY"}}
	return &items, nil
}

func (client *subtreePagedTestClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	client.pageCalls++
	items := []driver.File{
		{FileID: "unexpected-1", Name: "unexpected-1.bin", Size: 1, Sha1: "ONE"},
		{FileID: "unexpected-2", Name: "unexpected-2.bin", Size: 1, Sha1: "TWO"},
	}
	return &items, nil
}

func subtreeTestSHA1(value string) string {
	digest := sha1.Sum([]byte(value))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func TestValidateRemoteSubtreeUsesPagedEarlyStopForUnexpectedGrowth(t *testing.T) {
	client := &subtreePagedTestClient{}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{{
		RelativePath: "node", RemotePresent: true, RemotePath: "/remote/node", RemoteID: "root", Kind: "directory",
	}}}
	if err := ValidateRemoteSubtree(client, plan, plan.Items[0]); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("unexpected paged subtree growth error = %v", err)
	}
	if client.listCalls != 0 || client.pageCalls != 1 {
		t.Fatalf("remote subtree collection calls: List=%d ListPage=%d, want 0/1", client.listCalls, client.pageCalls)
	}
}

func TestValidateRemoteSubtreeRejectsUnexpectedDeepEntry(t *testing.T) {
	client := &subtreeTestRemoteClient{lists: map[string][]driver.File{
		"root": {{FileID: "sub", Name: "sub", IsDirectory: true}},
		"sub":  {{FileID: "old", Name: "old.bin", Size: 3, Sha1: "OLD"}},
	}}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "node", RemotePresent: true, RemotePath: "/remote/node", RemoteID: "root", Kind: "directory"},
		{RelativePath: "node/sub", RemotePresent: true, RemotePath: "/remote/node/sub", RemoteID: "sub", Kind: "directory"},
		{RelativePath: "node/sub/old.bin", RemotePresent: true, RemotePath: "/remote/node/sub/old.bin", RemoteID: "old", Kind: "file", RemoteSize: 3, RemoteSHA1: "OLD"},
	}}
	root := plan.Items[0]
	if err := ValidateRemoteSubtree(client, plan, root); err != nil {
		t.Fatalf("unchanged remote subtree rejected: %v", err)
	}
	client.lists["sub"] = append(client.lists["sub"], driver.File{FileID: "new", Name: "new.bin", Size: 3, Sha1: "NEW"})
	if err := ValidateRemoteSubtree(client, plan, root); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("unexpected deep remote entry error = %v", err)
	}
}

func TestValidateRemoteSubtreeRejectsMissingAndChangedFile(t *testing.T) {
	basePlan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "node", RemotePresent: true, RemotePath: "/remote/node", RemoteID: "root", Kind: "directory"},
		{RelativePath: "node/old.bin", RemotePresent: true, RemotePath: "/remote/node/old.bin", RemoteID: "old", Kind: "file", RemoteSize: 3, RemoteSHA1: "OLD"},
	}}
	root := basePlan.Items[0]
	client := &subtreeTestRemoteClient{lists: map[string][]driver.File{"root": {}}}
	if err := ValidateRemoteSubtree(client, basePlan, root); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("missing remote descendant error = %v", err)
	}
	client.lists["root"] = []driver.File{{FileID: "old", Name: "old.bin", Size: 3, Sha1: "NEW"}}
	if err := ValidateRemoteSubtree(client, basePlan, root); err == nil || !strings.Contains(err.Error(), "changed content") {
		t.Fatalf("changed remote descendant error = %v", err)
	}
}

func TestValidateLocalSubtreeRejectsUnexpectedAndMissingEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "sub", "old.bin")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "node", LocalPresent: true, LocalPath: dir, Kind: "directory"},
		{RelativePath: "node/sub", LocalPresent: true, LocalPath: filepath.Join(dir, "sub"), Kind: "directory"},
		{RelativePath: "node/sub/old.bin", LocalPresent: true, LocalPath: oldPath, Kind: "file", LocalSize: 3, LocalModTimeUnixNano: oldInfo.ModTime().UnixNano(), LocalSHA1: subtreeTestSHA1("old")},
	}}
	rootItem := plan.Items[0]
	if err := ValidateLocalSubtree(plan, rootItem); err != nil {
		t.Fatalf("unchanged local subtree rejected: %v", err)
	}
	newPath := filepath.Join(dir, "sub", "new.bin")
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLocalSubtree(plan, rootItem); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("unexpected local descendant error = %v", err)
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLocalSubtree(plan, rootItem); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("missing local descendant error = %v", err)
	}
}

func TestValidateLocalSubtreeRejectsSameSizeSameMtimeContentRewrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(path, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "node", LocalPresent: true, LocalPath: dir, Kind: "directory"},
		{RelativePath: "node/payload.bin", LocalPresent: true, LocalPath: path, Kind: "file", LocalSize: 4, LocalModTimeUnixNano: info.ModTime().UnixNano(), LocalSHA1: subtreeTestSHA1("AAAA")},
	}}
	if err := ValidateLocalSubtree(plan, plan.Items[0]); err != nil {
		t.Fatalf("fresh content-addressed local subtree rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLocalSubtree(plan, plan.Items[0]); err == nil || !strings.Contains(err.Error(), "changed content") {
		t.Fatalf("same-size/same-mtime content rewrite error = %v", err)
	}
}

func TestValidateLocalSubtreeRejectsSymlinkDescendant(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "node", LocalPresent: true, LocalPath: dir, Kind: "directory"},
		{RelativePath: "node/link.bin", LocalPresent: true, LocalPath: link, Kind: "file", LocalSize: 1, LocalSHA1: subtreeTestSHA1("x")},
	}}
	if err := ValidateLocalSubtree(plan, plan.Items[0]); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("symlink descendant error = %v", err)
	}
}
