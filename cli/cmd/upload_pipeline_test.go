package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
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

func TestUploadSourceRequestsContentsForTrailingSeparator(t *testing.T) {
	if !uploadSourceRequestsContents("source/") {
		t.Fatal("trailing slash did not request contents mode")
	}
	backslash := uploadSourceRequestsContents(`source\\`)
	if runtime.GOOS == "windows" && !backslash {
		t.Fatal("Windows trailing backslash did not request contents mode")
	}
	if runtime.GOOS != "windows" && backslash {
		t.Fatal("non-Windows trailing backslash unexpectedly requested contents mode")
	}
	if uploadSourceRequestsContents("source") {
		t.Fatal("plain source path unexpectedly requested contents mode")
	}
}

func TestPrepareRecursiveUploadDestinationPreservesSourceDirectoryByDefault(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "20251004")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	destination, rootID, err := prepareRecursiveUploadDestination(client, "/data/research/idpc", "root", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if destination != "/data/research/idpc/20251004" || rootID == "" || rootID == "root" {
		t.Fatalf("unexpected preserved destination: path=%q id=%q", destination, rootID)
	}
	secondDestination, secondID, err := prepareRecursiveUploadDestination(client, "/data/research/idpc", "root", root, false)
	if err != nil {
		t.Fatal(err)
	}
	if secondDestination != destination || secondID != rootID {
		t.Fatalf("existing recursive upload root was not reused: first=(%q,%q) second=(%q,%q)", destination, rootID, secondDestination, secondID)
	}
	entries, _ := client.List("root")
	if len(*entries) != 1 || (*entries)[0].Name != "20251004" || !(*entries)[0].IsDirectory {
		t.Fatalf("unexpected remote parent contents: %#v", *entries)
	}
}

func TestPrepareRecursiveUploadDestinationContentsModeUsesParentDirectly(t *testing.T) {
	client := newFakeUploadTreeClient()
	destination, rootID, err := prepareRecursiveUploadDestination(client, "/data/research/idpc", "root", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if destination != "/data/research/idpc" || rootID != "root" {
		t.Fatalf("contents mode changed destination: path=%q id=%q", destination, rootID)
	}
	entries, _ := client.List("root")
	if len(*entries) != 0 {
		t.Fatalf("contents mode unexpectedly created a source root: %#v", *entries)
	}
}

func TestPrepareRecursiveUploadDestinationRejectsFileConflict(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	client.addFile("root", "source", 1, "SHA")
	if _, _, err := prepareRecursiveUploadDestination(client, "/remote", "root", root, false); err == nil {
		t.Fatal("expected same-name remote file conflict")
	}
}

func TestExecuteRecursiveUploadPreservesSourceDirectoryRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child.bin"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		client.addFile(dirID, name, size, "SHA")
		return uploadpkg.Result{SHA1: "SHA", BytesUploaded: size}, nil
	}}
	summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", false, false, "", uploadpkg.DefaultOptions(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.RemoteDir != "/remote/source" || summary.SucceededCount != 1 {
		t.Fatalf("unexpected preserved upload summary: %#v", summary)
	}
	rootEntries, _ := client.List("root")
	if len(*rootEntries) != 1 || (*rootEntries)[0].Name != "source" || !(*rootEntries)[0].IsDirectory {
		t.Fatalf("source root was not created under destination: %#v", *rootEntries)
	}
	childEntries, _ := client.List((*rootEntries)[0].FileID)
	if len(*childEntries) != 1 || (*childEntries)[0].Name != "child.bin" {
		t.Fatalf("source contents were not uploaded below preserved root: %#v", *childEntries)
	}
}

func TestExecuteRecursiveUploadSkipsFreshIdenticalRemoteFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := uploadpkg.PrepareFileDigest(file, 7)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	client.addFile("root", "a.bin", 7, digest.SHA1)
	calls := 0
	deps := uploadPipelineDeps{uploadFile: func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		return uploadpkg.Result{}, errors.New("identical remote file should have been skipped")
	}}
	summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, false, "", uploadpkg.DefaultOptions(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || summary.SucceededCount != 1 || summary.UploadedCount != 0 || summary.VerifiedCount != 1 || summary.SkippedCount != 1 || summary.TransferredBytes != 0 {
		t.Fatalf("unexpected identical-file result: calls=%d summary=%#v", calls, summary)
	}
}

func TestExecuteRecursiveUploadReusesVerificationDigestWhenRemoteDiffers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := uploadpkg.PrepareFileDigest(file, 7)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	client.addFile("root", "a.bin", 7, "0000000000000000000000000000000000000000")
	calls := 0
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		if options.PreparedDigest == nil || options.PreparedDigest.SHA1 != digest.SHA1 || options.PreparedDigest.PreID != digest.PreID {
			t.Fatalf("verification digest was not reused: %#v", options.PreparedDigest)
		}
		client.addFile(dirID, name, size, digest.SHA1)
		return uploadpkg.Result{SHA1: digest.SHA1, BytesUploaded: size}, nil
	}}
	summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, false, "", uploadpkg.DefaultOptions(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || summary.UploadedCount != 1 || summary.VerifiedCount != 0 || summary.SkippedCount != 0 || summary.TransferredBytes != 7 {
		t.Fatalf("unexpected mismatched-file result: calls=%d summary=%#v", calls, summary)
	}
}

func TestExecuteRecursiveUploadActiveFileResumeStateBypassesFreshRemoteSkip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := uploadpkg.PrepareFileDigest(file, 7)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	sessionPath, partsDir, err := deriveTransferSessionPaths("upload", root, "/remote", "")
	if err != nil {
		t.Fatal(err)
	}
	resumePath := uploadResumePathForRelative(partsDir, "a.bin")
	if err := os.MkdirAll(filepath.Dir(resumePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePath, []byte("active resume state"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	client.addFile("root", "a.bin", 7, digest.SHA1)
	calls := 0
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		if options.ResumePath != resumePath {
			t.Fatalf("resume path was not preserved: got %q want %q", options.ResumePath, resumePath)
		}
		if options.PreparedDigest != nil {
			t.Fatalf("active resume state should bypass fresh pre-verification digest: %#v", options.PreparedDigest)
		}
		return uploadpkg.Result{SHA1: digest.SHA1, BytesUploaded: size, Resumed: true}, nil
	}}
	summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, true, sessionPath, uploadpkg.DefaultOptions(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || summary.UploadedCount != 1 || summary.SkippedCount != 0 || summary.ResumedCount != 1 {
		t.Fatalf("active resume state was incorrectly short-circuited: calls=%d summary=%#v", calls, summary)
	}
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

	first, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, true, "", options, deps)
	if !errors.Is(err, errUploadIncomplete) {
		t.Fatalf("expected incomplete first run, got summary=%#v err=%v", first, err)
	}
	if first.FileCount != 2 || first.SucceededCount != 1 || first.FailedCount != 1 || first.SessionPath == "" {
		t.Fatalf("unexpected first summary: %#v", first)
	}
	if _, err := os.Stat(first.SessionPath); err != nil {
		t.Fatalf("recursive upload session not preserved: %v", err)
	}
	partsDir := filepath.Join(filepath.Dir(first.SessionPath), "parts")
	if len(resumePaths) != 2 || resumePaths[0] == "" || resumePaths[1] == "" || filepath.Dir(resumePaths[0]) != partsDir {
		t.Fatalf("per-file upload resume state was not rooted below session parts dir: %v", resumePaths)
	}

	second, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, true, "", options, deps)
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

func TestExecuteRecursiveUploadMapsPerFileProgressIntoTreeProgress(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.bin"), []byte("bbbbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newFakeUploadTreeClient()
	options := uploadpkg.DefaultOptions()
	var statuses []string
	var progress []int64
	options.Progress = func(message string) { statuses = append(statuses, message) }
	options.ProgressBytes = func(completed, total int64) {
		if total != 10 {
			t.Fatalf("unexpected recursive progress total: %d", total)
		}
		progress = append(progress, completed)
	}
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, _ string, name string, size int64, _ *os.File, fileOptions uploadpkg.Options) (uploadpkg.Result, error) {
		if fileOptions.Progress != nil {
			fileOptions.Progress("Checking 115 rapid upload...")
		}
		if fileOptions.ProgressBytes != nil {
			fileOptions.ProgressBytes(size/2, size)
			fileOptions.ProgressBytes(size, size)
		}
		return uploadpkg.Result{BytesUploaded: size, SHA1: "SHA-" + name}, nil
	}}

	summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, false, "", options, deps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SucceededCount != 2 || len(progress) == 0 || progress[len(progress)-1] != 10 {
		t.Fatalf("unexpected recursive progress summary=%#v progress=%v", summary, progress)
	}
	for _, value := range progress {
		if value < 0 || value > 10 {
			t.Fatalf("tree progress escaped logical total: %v", progress)
		}
	}
	joined := strings.Join(statuses, "\n")
	if !strings.Contains(joined, "[1/2] a.bin — Checking 115 rapid upload...") || !strings.Contains(joined, "[2/2] b.bin — Checking 115 rapid upload...") {
		t.Fatalf("per-file statuses were not folded into one tree progress stream: %v", statuses)
	}
}

func TestExecuteRecursiveUploadDistributesSequentialFilesAcrossInterfaces(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin", "c.bin", "d.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("payload-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := newFakeUploadTreeClient()
	options := uploadpkg.DefaultOptions()
	paths := []transfer.NetworkPath{
		{InterfaceName: "NIC-1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")},
		{InterfaceName: "NIC-2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.0.2")},
	}
	entered := make(chan string, 4)
	release := make(chan struct{})
	var selectorsMu sync.Mutex
	selectors := map[string]string{}
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, _ string, name string, size int64, _ *os.File, fileOptions uploadpkg.Options) (uploadpkg.Result, error) {
		if name == "a.bin" {
			if fileOptions.Interfaces != "auto" {
				t.Fatalf("discovery file should use the original selector, got %q", fileOptions.Interfaces)
			}
			fileOptions.Compatibility.ObserveNetworkPaths(paths)
			fileOptions.Compatibility.RequireSequential()
			return uploadpkg.Result{SHA1: "SHA-A", BytesUploaded: size, Sequential: true}, nil
		}
		selectorsMu.Lock()
		selectors[name] = fileOptions.Interfaces
		selectorsMu.Unlock()
		entered <- name
		<-release
		return uploadpkg.Result{SHA1: "SHA-" + name, BytesUploaded: size, Sequential: true}, nil
	}}

	type uploadResult struct {
		summary uploadCommandSummary
		err     error
	}
	done := make(chan uploadResult, 1)
	go func() {
		summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, false, "", options, deps)
		done <- uploadResult{summary: summary, err: err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("remaining files were not uploaded concurrently across interfaces")
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.summary.SucceededCount != 4 || result.summary.FailedCount != 0 {
		t.Fatalf("unexpected distributed upload summary: %#v", result.summary)
	}
	selectorsMu.Lock()
	defer selectorsMu.Unlock()
	seen := map[string]bool{}
	for name, selector := range selectors {
		if selector != "10.0.0.1" && selector != "10.0.0.2" {
			t.Fatalf("file %s was not pinned to a discovered physical interface: %q", name, selector)
		}
		seen[selector] = true
	}
	if len(seen) != 2 {
		t.Fatalf("remaining files did not use both interfaces: selectors=%v", selectors)
	}
}

func TestExecuteRecursiveUploadUsesMultipleFileConnectionsOnOneSequentialInterface(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("payload-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := newFakeUploadTreeClient()
	options := uploadpkg.DefaultOptions()
	options.WorkersPerInterface = 2
	path := transfer.NetworkPath{InterfaceName: "NIC-1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	deps := uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, _ string, name string, size int64, _ *os.File, fileOptions uploadpkg.Options) (uploadpkg.Result, error) {
		if name == "a.bin" {
			fileOptions.Compatibility.ObserveNetworkPaths([]transfer.NetworkPath{path})
			fileOptions.Compatibility.RequireSequential()
			return uploadpkg.Result{SHA1: "SHA-A", BytesUploaded: size, Sequential: true}, nil
		}
		if fileOptions.Interfaces != path.LocalIP.String() {
			t.Fatalf("single-NIC worker was not pinned to the discovered path: %q", fileOptions.Interfaces)
		}
		entered <- struct{}{}
		<-release
		return uploadpkg.Result{SHA1: "SHA-" + name, BytesUploaded: size, Sequential: true}, nil
	}}
	type uploadResult struct {
		summary uploadCommandSummary
		err     error
	}
	done := make(chan uploadResult, 1)
	go func() {
		summary, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", true, false, "", options, deps)
		done <- uploadResult{summary: summary, err: err}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("single physical interface did not use both recursive upload connection slots")
		}
	}
	close(release)
	result := <-done
	if result.err != nil || result.summary.SucceededCount != 3 || result.summary.FailedCount != 0 {
		t.Fatalf("unexpected single-interface distributed result: summary=%#v err=%v", result.summary, result.err)
	}
}

func TestRecursiveUploadPinnedWorkerFallsBackThroughOriginalSelector(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.bin")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	source := localUploadFile{FullPath: path, RelativePath: "file.bin", Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}
	options := uploadpkg.DefaultOptions()
	var callsMu sync.Mutex
	var selectors []string
	var retries []int
	processor := &recursiveUploadFileProcessor{
		deps: uploadPipelineDeps{uploadFile: func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, fileOptions uploadpkg.Options) (uploadpkg.Result, error) {
			callsMu.Lock()
			selectors = append(selectors, fileOptions.Interfaces)
			retries = append(retries, fileOptions.Retries)
			callsMu.Unlock()
			if fileOptions.Interfaces != "auto" {
				return uploadpkg.Result{}, driver.ErrUploadVerificationFailed
			}
			return uploadpkg.Result{SHA1: "SHA", BytesUploaded: size, Sequential: true}, nil
		}},
		directoryIDs: map[string]string{"": "root"},
		listings:     map[string][]driver.File{"": {}},
		sessionFiles: map[string]transfer.TransferTreeSessionFile{},
		options:      options,
		progress:     newRecursiveUploadProgress(source.Size, nil),
		fileCount:    1,
	}
	paths := []transfer.NetworkPath{
		{InterfaceName: "NIC-1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.0.1")},
		{InterfaceName: "NIC-2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.0.2")},
	}
	outcomes, err := processor.processConcurrent(context.Background(), []localUploadFile{source}, 0, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || !outcomes[0].Success {
		t.Fatalf("pinned worker did not recover through original selector: %#v", outcomes)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if len(selectors) != 2 || selectors[0] == "auto" || selectors[1] != "auto" {
		t.Fatalf("unexpected pinned/failover selector sequence: %v", selectors)
	}
	if len(retries) != 2 || retries[0] != 0 || retries[1] != options.Retries {
		t.Fatalf("pinned/failover retry budgets were not separated: %v", retries)
	}
}

func TestScanLocalUploadTreeRecursivelyExcludes115DriverTransferState(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "level1", "level2", "level3")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	keepFiles := []string{
		filepath.Join(root, "root.bin"),
		filepath.Join(deep, "keep.bin"),
		filepath.Join(deep, "notes.115driver-project.json"),
	}
	for _, path := range keepFiles {
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	stateFiles := []string{
		filepath.Join(root, ".root.115driver-upload-aaa.session.json"),
		filepath.Join(root, "level1", ".nested.115driver-download-bbb.session.json"),
		filepath.Join(deep, ".deep.115driver-upload-ccc.session.json"),
		filepath.Join(deep, "..deep.115driver-upload-ccc.session.json.123456789"),
	}
	for _, path := range stateFiles {
		if err := os.WriteFile(path, []byte("state"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	partsDir := filepath.Join(root, "level1", "level2", ".child.115driver-upload-ddd.session.json.parts")
	if err := os.MkdirAll(filepath.Join(partsDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partsDir, "nested", "should-never-be-scanned.upload.json"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}

	tree, err := scanLocalUploadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	gotFiles := make([]string, 0, len(tree.Files))
	for _, file := range tree.Files {
		gotFiles = append(gotFiles, file.RelativePath)
	}
	wantFiles := []string{
		filepath.Join("level1", "level2", "level3", "keep.bin"),
		filepath.Join("level1", "level2", "level3", "notes.115driver-project.json"),
		"root.bin",
	}
	if fmt.Sprint(gotFiles) != fmt.Sprint(wantFiles) {
		t.Fatalf("unexpected recursively scanned files: got=%v want=%v", gotFiles, wantFiles)
	}
	for _, relative := range tree.Directories {
		if strings.Contains(relative, ".115driver-") {
			t.Fatalf("transfer-state directory leaked into upload tree: %q", relative)
		}
	}
}

func TestIs115DriverTransferStateNameDoesNotOvermatchUserFiles(t *testing.T) {
	tests := []struct {
		name  string
		isDir bool
		want  bool
	}{
		{name: ".foo.115driver-upload-a.session.json", want: true},
		{name: "..foo.115driver-upload-a.session.json.12345", want: true},
		{name: ".foo.115driver-download-a.session.json", want: true},
		{name: ".foo.115driver-upload-a.session.json.parts", isDir: true, want: true},
		{name: "notes.115driver-project.json", want: false},
		{name: "archive.115driver-project.parts-old", isDir: true, want: false},
		{name: "ordinary.session.json", want: false},
	}
	for _, test := range tests {
		if got := is115DriverTransferStateName(test.name, test.isDir); got != test.want {
			t.Errorf("is115DriverTransferStateName(%q, %v)=%v want %v", test.name, test.isDir, got, test.want)
		}
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
	_, err := executeRecursiveUpload(context.Background(), client, nil, root, "/remote", "root", false, true, insideSession, options, uploadPipelineDeps{uploadFile: func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		return uploadpkg.Result{}, nil
	}})
	if !errors.Is(err, errUploadUsage) {
		t.Fatalf("expected in-tree session rejection, got %v", err)
	}
	entries, _ := client.List("root")
	if len(*entries) != 0 {
		t.Fatalf("invalid session path created remote source root before validation: %#v", *entries)
	}
}

func TestRecursiveUploadModesUseDistinctDefaultSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	preservedDestination, err := recursiveUploadDestinationPath("/remote", root, false)
	if err != nil {
		t.Fatal(err)
	}
	contentsDestination, err := recursiveUploadDestinationPath("/remote", root, true)
	if err != nil {
		t.Fatal(err)
	}
	preservedSession, _, err := deriveTransferSessionPaths("upload", root, preservedDestination, "")
	if err != nil {
		t.Fatal(err)
	}
	contentsSession, _, err := deriveTransferSessionPaths("upload", root, contentsDestination, "")
	if err != nil {
		t.Fatal(err)
	}
	if preservedSession == contentsSession {
		t.Fatalf("directory and contents modes share a default session: %q", preservedSession)
	}
}

func TestDeriveTransferSessionPathsWindowsCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path identity test")
	}
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if len(root) < 2 || root[1] != ':' {
		t.Skipf("not a drive-letter path: %q", root)
	}
	alternate := strings.ToLower(root[:1]) + root[1:]
	first, _, err := deriveTransferSessionPaths("upload", root, "/remote", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := deriveTransferSessionPaths("upload", alternate, "/remote", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(first, second) || filepath.Base(first) != filepath.Base(second) {
		t.Fatalf("Windows path casing split transfer sessions: %q != %q", first, second)
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
