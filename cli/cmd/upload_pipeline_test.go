package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeUploadTreeClient struct {
	mu      sync.Mutex
	lists   map[string][]driver.File
	nextDir int
}

func newFakeUploadTreeClient() *fakeUploadTreeClient {
	return &fakeUploadTreeClient{lists: map[string][]driver.File{"root": {}}, nextDir: 1}
}

func (client *fakeUploadTreeClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *fakeUploadTreeClient) Mkdir(parentID, name string) (string, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	id := fmt.Sprintf("d%d", client.nextDir)
	client.nextDir++
	client.lists[parentID] = append(client.lists[parentID], driver.File{FileID: id, Name: name, IsDirectory: true})
	client.lists[id] = []driver.File{}
	return id, nil
}

func (client *fakeUploadTreeClient) addFile(dirID, name string, size int64, sha1 string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.lists[dirID] = append(client.lists[dirID], driver.File{Name: name, Size: size, Sha1: sha1})
}

func TestExecuteRecursiveUploadSessionSkipsCompletedFilesOnSecondRun(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), []byte("bbbbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	options := uploadpkg.DefaultOptions()
	var callsMu sync.Mutex
	calls := map[string]int{}
	firstBFailure := true
	var resumePaths []string
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		callsMu.Lock()
		calls[name]++
		resumePaths = append(resumePaths, options.ResumePath)
		callsMu.Unlock()
		if name == "b.bin" && firstBFailure {
			firstBFailure = false
			return uploadpkg.Result{SHA1: "SHA-B"}, errors.New("simulated disconnect")
		}
		sha1 := "SHA-A"
		if name == "b.bin" {
			sha1 = "SHA-B"
		}
		client.addFile(dirID, name, size, sha1)
		return uploadpkg.Result{SHA1: sha1, BytesUploaded: size, Resumed: name == "b.bin" && calls[name] > 1}, nil
	}}

	first, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, "", options, deps)
	if !errors.Is(err, errUploadIncomplete) {
		t.Fatalf("expected incomplete first run, got summary=%#v err=%v", first, err)
	}
	if first.FileCount != 2 || first.SucceededCount != 1 || first.FailedCount != 1 || first.SessionPath == "" {
		t.Fatalf("unexpected first summary: %#v", first)
	}
	if _, err := os.Stat(first.SessionPath); err != nil {
		t.Fatalf("recursive upload session not preserved: %v", err)
	}
	_, partsDir, err := deriveTransferSessionPaths("upload", root, "/remote", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumePaths) != 2 || resumePaths[0] == "" || resumePaths[1] == "" || filepath.Dir(resumePaths[0]) != partsDir {
		t.Fatalf("per-file upload resume state was not rooted below session parts dir: %v", resumePaths)
	}

	second, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, "", options, deps)
	if err != nil {
		t.Fatal(err)
	}
	if second.SucceededCount != 2 || second.FailedCount != 0 || second.ResumedCount < 1 || second.SessionPath != "" {
		t.Fatalf("unexpected resumed summary: %#v", second)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls["a.bin"] != 1 || calls["b.bin"] != 2 {
		t.Fatalf("completed file was uploaded again: %#v", calls)
	}
	if _, err := os.Stat(first.SessionPath); !os.IsNotExist(err) {
		t.Fatalf("successful session was not cleaned: %v", err)
	}
	if _, err := os.Stat(partsDir); !os.IsNotExist(err) {
		t.Fatalf("successful per-file state directory was not cleaned: %v", err)
	}

	rootEntries, _ := client.List("root")
	var subID, emptyID string
	for _, entry := range *rootEntries {
		if entry.Name == "sub" && entry.IsDirectory {
			subID = entry.FileID
		}
		if entry.Name == "empty" && entry.IsDirectory {
			emptyID = entry.FileID
		}
	}
	if subID == "" || emptyID == "" {
		t.Fatalf("recursive upload did not preserve directory hierarchy: %#v", *rootEntries)
	}
}

func TestScanLocalUploadTreeRejectsSymlinkAndSpecialSessionInsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := scanLocalUploadTree(root); !errors.Is(err, errUploadUsage) {
			t.Fatalf("expected symlink rejection, got %v", err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	options := uploadpkg.DefaultOptions()
	insideSession := filepath.Join(root, "session.json")
	_, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, insideSession, options, uploadPipelineDeps{uploadFile: func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		return uploadpkg.Result{}, nil
	}})
	if !errors.Is(err, errUploadUsage) {
		t.Fatalf("expected in-tree session rejection, got %v", err)
	}
}

func TestDeriveTransferSessionPathsStableAndOutsideAnchor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	first, parts, err := deriveTransferSessionPaths("upload", root, "/remote", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := deriveTransferSessionPaths("upload", root, "/remote", "")
	if err != nil || first != second {
		t.Fatalf("session derivation is not deterministic: %q %q %v", first, second, err)
	}
	inside, err := pathIsWithin(root, first)
	if err != nil || inside {
		t.Fatalf("default session should be outside source: path=%q inside=%v err=%v", first, inside, err)
	}
	if filepath.Ext(parts) != ".parts" {
		t.Fatalf("unexpected parts directory: %q", parts)
	}
}
