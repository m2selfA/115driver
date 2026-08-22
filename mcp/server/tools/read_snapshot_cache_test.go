package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type mcpOverlappingTreeTestClient struct {
	listCalls      map[string]int
	failFirstPage  string
	failedPageOnce bool
}

func newMCPOverlappingTreeTestClient() *mcpOverlappingTreeTestClient {
	return &mcpOverlappingTreeTestClient{listCalls: make(map[string]int)}
}

func (c *mcpOverlappingTreeTestClient) DirName2CID(remotePath string) (*driver.APIGetDirIDResp, error) {
	switch remotePath {
	case "A":
		return &driver.APIGetDirIDResp{CategoryID: driver.IntString("a")}, nil
	case "A/B":
		return &driver.APIGetDirIDResp{CategoryID: driver.IntString("b")}, nil
	default:
		return nil, fmt.Errorf("unexpected test path %q", remotePath)
	}
}

func (c *mcpOverlappingTreeTestClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	key := fmt.Sprintf("%s:%d:%d", dirID, offset, limit)
	c.listCalls[key]++
	if c.failFirstPage == key && !c.failedPageOnce {
		c.failedPageOnce = true
		return nil, errors.New("synthetic first-page failure")
	}
	if offset != 0 {
		empty := []driver.File{}
		return &empty, nil
	}
	var files []driver.File
	switch dirID {
	case "a":
		files = []driver.File{{FileID: "b", ParentID: "a", Name: "B", IsDirectory: true}}
	case "b":
		files = []driver.File{{FileID: "f", ParentID: "b", Name: "file.bin", Size: 7, PickCode: "pick-f", Sha1: "ABC"}}
	default:
		return nil, fmt.Errorf("unexpected test directory %q", dirID)
	}
	return &files, nil
}

func (c *mcpOverlappingTreeTestClient) GetFile(fileID string) (*driver.File, error) {
	if fileID != "f" {
		return nil, fmt.Errorf("unexpected test file %q", fileID)
	}
	return &driver.File{FileID: "f", ParentID: "b", Name: "file.bin", Size: 7, PickCode: "pick-f", Sha1: "ABC"}, nil
}

func TestListTreeReusesReadOnlyPagesAcrossOverlappingRoots(t *testing.T) {
	client := newMCPOverlappingTreeTestClient()
	result, err := listMCPRemoteTree(context.Background(), client, ListTreeArgs{Paths: []string{"A", "A/B"}, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 2 || result.Failed != 0 || result.NodesVisited != 3 {
		t.Fatalf("unexpected overlapping list_tree result: %#v", result)
	}
	if got := client.listCalls["a:0:500"]; got != 1 {
		t.Fatalf("root A page calls = %d, want 1", got)
	}
	if got := client.listCalls["b:0:500"]; got != 1 {
		t.Fatalf("overlapping B page calls = %d, want one request-scoped snapshot", got)
	}
	if len(result.Items[0].Entries) != 2 || len(result.Items[1].Entries) != 1 {
		t.Fatalf("page reuse changed per-root output semantics: %#v", result.Items)
	}
}

func TestSummarizeUsageReusesReadOnlyPagesAcrossOverlappingRoots(t *testing.T) {
	client := newMCPOverlappingTreeTestClient()
	result, err := summarizeMCPUsage(context.Background(), client, SummarizeUsageArgs{Paths: []string{"A", "A/B"}, MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 2 || result.Failed != 0 || result.NodesVisited != 3 {
		t.Fatalf("unexpected overlapping summarize_usage result: %#v", result)
	}
	if got := client.listCalls["b:0:500"]; got != 1 {
		t.Fatalf("overlapping B page calls = %d, want one request-scoped snapshot", got)
	}
	if result.Items[0].Data == nil || result.Items[0].Data.Size != 7 || result.Items[0].Data.Files != 1 || result.Items[0].Data.Directories != 1 {
		t.Fatalf("unexpected A usage summary: %#v", result.Items[0])
	}
	if result.Items[1].Data == nil || result.Items[1].Data.Size != 7 || result.Items[1].Data.Files != 1 || result.Items[1].Data.Directories != 0 {
		t.Fatalf("unexpected A/B usage summary: %#v", result.Items[1])
	}
}

func TestReadPageSnapshotDoesNotCacheErrors(t *testing.T) {
	client := newMCPOverlappingTreeTestClient()
	client.failFirstPage = "b:0:500"
	cache := newMCPReadPageCache()

	if _, err := cache.listPage(client, "b", 0, 500, driver.WithRecordOpenTime(false)); err == nil {
		t.Fatal("expected first synthetic page failure")
	}
	files, err := cache.listPage(client, "b", 0, 500, driver.WithRecordOpenTime(false))
	if err != nil {
		t.Fatalf("successful retry after uncached failure: %v", err)
	}
	if files == nil || len(*files) != 1 || (*files)[0].FileID != "f" {
		t.Fatalf("unexpected retry page: %#v", files)
	}
	if got := client.listCalls["b:0:500"]; got != 2 {
		t.Fatalf("failed page was cached: underlying calls=%d, want 2", got)
	}
}

type mcpSnapshotBlockingClient struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	files   []driver.File
	err     error
}

func (c *mcpSnapshotBlockingClient) ListPage(_ string, _, _ int64, _ ...driver.ListOption) (*[]driver.File, error) {
	if c.calls.Add(1) == 1 && c.entered != nil {
		close(c.entered)
	}
	if c.release != nil {
		<-c.release
	}
	files := cloneMCPReadFiles(c.files)
	return &files, c.err
}

func TestReadPageSnapshotSingleFlightsConcurrentMisses(t *testing.T) {
	client := &mcpSnapshotBlockingClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		files:   []driver.File{{FileID: "f", Name: "file.bin"}},
	}
	cache := newMCPReadPageCache()
	const workers = 64
	start := make(chan struct{})
	results := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			ready.Done()
			<-start
			files, err := cache.listPage(client, "d", 0, 500, driver.WithRecordOpenTime(false))
			if err == nil && (files == nil || len(*files) != 1 || (*files)[0].FileID != "f") {
				err = fmt.Errorf("unexpected page: %#v", files)
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	<-client.entered
	// Keep the owner blocked long enough for all contenders to observe the
	// in-flight entry. This test is intentionally much longer than a scheduler
	// quantum but still tiny compared with network I/O.
	time.Sleep(50 * time.Millisecond)
	if got := client.calls.Load(); got != 1 {
		close(client.release)
		t.Fatalf("concurrent cache miss reached underlying ListPage %d times, want 1", got)
	}
	close(client.release)
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("single-flight completed with %d underlying calls, want 1", got)
	}
}

func TestReadPageSnapshotSingleFlightsErrorsButDoesNotPersistThem(t *testing.T) {
	client := &mcpSnapshotBlockingClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("synthetic concurrent page failure"),
	}
	cache := newMCPReadPageCache()
	const workers = 32
	start := make(chan struct{})
	results := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			ready.Done()
			<-start
			_, err := cache.listPage(client, "d", 0, 500, driver.WithRecordOpenTime(false))
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	<-client.entered
	time.Sleep(50 * time.Millisecond)
	if got := client.calls.Load(); got != 1 {
		close(client.release)
		t.Fatalf("concurrent failing miss reached underlying ListPage %d times, want 1", got)
	}
	close(client.release)
	for i := 0; i < workers; i++ {
		if err := <-results; err == nil || !strings.Contains(err.Error(), "synthetic concurrent page failure") {
			t.Fatalf("concurrent waiter error = %v", err)
		}
	}

	// The completed error must not become a persistent cache entry. A later
	// caller must get a real retry and may succeed.
	client.err = nil
	client.files = []driver.File{{FileID: "recovered", Name: "recovered.bin"}}
	client.release = nil
	files, err := cache.listPage(client, "d", 0, 500, driver.WithRecordOpenTime(false))
	if err != nil || files == nil || len(*files) != 1 || (*files)[0].FileID != "recovered" {
		t.Fatalf("retry after single-flighted error = %#v, %v", files, err)
	}
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("error retry underlying calls=%d, want 2", got)
	}
}

func TestReadPageSnapshotDeepCopiesNestedLabels(t *testing.T) {
	client := &mcpSnapshotBlockingClient{files: []driver.File{{
		FileID: "f", Name: "file.bin", Labels: []*driver.Label{{ID: "l1", Name: "original", Color: 2}},
	}}}
	cache := newMCPReadPageCache()
	first, err := cache.listPage(client, "d", 0, 500, driver.WithRecordOpenTime(false))
	if err != nil || first == nil || len(*first) != 1 || len((*first)[0].Labels) != 1 {
		t.Fatalf("unexpected first cached page: %#v err=%v", first, err)
	}
	(*first)[0].Labels[0].Name = "mutated"
	(*first)[0].Labels = append((*first)[0].Labels, &driver.Label{ID: "extra", Name: "extra"})

	second, err := cache.listPage(client, "d", 0, 500, driver.WithRecordOpenTime(false))
	if err != nil || second == nil || len(*second) != 1 {
		t.Fatalf("unexpected second cached page: %#v err=%v", second, err)
	}
	if got := (*second)[0].Labels; len(got) != 1 || got[0] == nil || got[0].Name != "original" {
		t.Fatalf("nested label mutation leaked into cached snapshot: %#v", got)
	}
	if client.calls.Load() != 1 {
		t.Fatalf("deep-copy check unexpectedly refetched page %d times", client.calls.Load())
	}
}

type mcpSnapshotCountingClient struct {
	mu    sync.Mutex
	calls map[string]int
	pages map[string][]driver.File
}

func (c *mcpSnapshotCountingClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
	key := fmt.Sprintf("%s:%d:%d", dirID, offset, limit)
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[key]++
	page := cloneMCPReadFiles(c.pages[key])
	c.mu.Unlock()
	return &page, nil
}

func (c *mcpSnapshotCountingClient) callCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

func TestReadPageSnapshotCacheBudgetBoundsPersistentSnapshotsWithoutChangingReads(t *testing.T) {
	client := &mcpSnapshotCountingClient{pages: map[string][]driver.File{
		"a:0:2": {{FileID: "a1"}, {FileID: "a2"}},
		"b:0:2": {{FileID: "b1"}, {FileID: "b2"}},
	}}
	cache := newMCPReadPageCacheWithLimit(2)

	for _, dirID := range []string{"a", "b", "a", "b"} {
		files, err := cache.listPage(client, dirID, 0, 2, driver.WithRecordOpenTime(false))
		if err != nil || files == nil || len(*files) != 2 {
			t.Fatalf("cache-budget read %q = %#v, %v", dirID, files, err)
		}
	}
	if got := client.callCount("a:0:2"); got != 1 {
		t.Fatalf("first page inside cache budget was refetched %d times, want 1", got)
	}
	if got := client.callCount("b:0:2"); got != 2 {
		t.Fatalf("page beyond cache budget underlying calls=%d, want 2", got)
	}
	if cache.cachedFiles != 2 || len(cache.pages) != 1 {
		t.Fatalf("cache budget state = cachedFiles=%d pages=%d, want 2/1", cache.cachedFiles, len(cache.pages))
	}
}

func TestReadPageSnapshotCachesEmptyTailWithoutConsumingFileBudget(t *testing.T) {
	client := &mcpSnapshotCountingClient{pages: map[string][]driver.File{"tail:500:500": {}}}
	cache := newMCPReadPageCacheWithLimit(0)
	for i := 0; i < 2; i++ {
		files, err := cache.listPage(client, "tail", 500, 500, driver.WithRecordOpenTime(false))
		if err != nil || files == nil || len(*files) != 0 {
			t.Fatalf("empty tail read %d = %#v, %v", i, files, err)
		}
	}
	if got := client.callCount("tail:500:500"); got != 1 {
		t.Fatalf("empty tail was not cached: calls=%d", got)
	}
	if cache.cachedFiles != 0 || len(cache.pages) != 1 {
		t.Fatalf("empty tail cache state = cachedFiles=%d pages=%d", cache.cachedFiles, len(cache.pages))
	}
}

type mcpSnapshotOptionClient struct {
	mu    sync.Mutex
	calls int
}

func (c *mcpSnapshotOptionClient) ListPage(_ string, _, _ int64, opts ...driver.ListOption) (*[]driver.File, error) {
	parsed := driver.DefaultListOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(parsed)
		}
	}
	fingerprint := strings.Join(parsed.ApiURLs, " -> ")
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	files := []driver.File{{FileID: fingerprint, Name: fingerprint}}
	return &files, nil
}

func (c *mcpSnapshotOptionClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestReadPageSnapshotKeySeparatesAPIURLFallbackStrategies(t *testing.T) {
	client := &mcpSnapshotOptionClient{}
	cache := newMCPReadPageCache()
	strategies := [][]string{
		{"https://a.invalid", "https://b.invalid"},
		{"https://b.invalid", "https://a.invalid"},
		{"https://a.invalid", "https://b.invalid"},
	}
	for i, urls := range strategies {
		files, err := cache.listPage(client, "d", 0, 100, driver.WithApiURLs(urls...))
		if err != nil || files == nil || len(*files) != 1 {
			t.Fatalf("API strategy read %d = %#v, %v", i, files, err)
		}
		if got, want := (*files)[0].FileID, strings.Join(urls, " -> "); got != want {
			t.Fatalf("API strategy read %d returned %q, want %q", i, got, want)
		}
	}
	if got := client.callCount(); got != 2 {
		t.Fatalf("API fallback strategies shared the wrong cache key: underlying calls=%d, want 2", got)
	}
}

func TestReadPageSnapshotFetchBudgetStopsBeforeNetworkAndCacheHitsRemainUsable(t *testing.T) {
	client := &mcpSnapshotCountingClient{pages: map[string][]driver.File{
		"a:0:2": {{FileID: "a1"}, {FileID: "a2"}},
		"b:0:2": {{FileID: "b1"}, {FileID: "b2"}},
	}}
	cache := newMCPReadPageCacheWithBudgets(2, 2)

	first, err := cache.listPage(client, "a", 0, 2, driver.WithRecordOpenTime(false))
	if err != nil || first == nil || len(*first) != 2 {
		t.Fatalf("first budgeted read = %#v, %v", first, err)
	}
	// The budget is fully reserved, but a cached repeat must remain usable and
	// must not consume another read reservation.
	second, err := cache.listPage(client, "a", 0, 2, driver.WithRecordOpenTime(false))
	if err != nil || second == nil || len(*second) != 2 {
		t.Fatalf("cached read after budget exhaustion = %#v, %v", second, err)
	}
	if _, err := cache.listPage(client, "b", 0, 2, driver.WithRecordOpenTime(false)); err == nil || !strings.Contains(err.Error(), "page budget exhausted") {
		t.Fatalf("new page beyond fetch budget error = %v", err)
	}
	if got := client.callCount("a:0:2"); got != 1 {
		t.Fatalf("cached page underlying calls=%d, want 1", got)
	}
	if got := client.callCount("b:0:2"); got != 0 {
		t.Fatalf("fetch budget was enforced after network: b calls=%d", got)
	}
	if cache.fetchedEntries != 2 {
		t.Fatalf("fetch budget accounting=%d, want 2", cache.fetchedEntries)
	}
}

func TestReadPageSnapshotFetchBudgetCountsFailedNetworkAttempts(t *testing.T) {
	client := &mcpSnapshotBlockingClient{err: errors.New("synthetic network failure")}
	cache := newMCPReadPageCacheWithBudgets(0, 2)
	if _, err := cache.listPage(client, "a", 0, 2, driver.WithRecordOpenTime(false)); err == nil {
		t.Fatal("expected first network failure")
	}
	if _, err := cache.listPage(client, "a", 0, 2, driver.WithRecordOpenTime(false)); err == nil || !strings.Contains(err.Error(), "page budget exhausted") {
		t.Fatalf("retry after consumed fetch budget = %v", err)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("failed request did not consume remote read budget: calls=%d", got)
	}
}

func TestReadPageSnapshotScopeDoesNotCrossToolCalls(t *testing.T) {
	client := newMCPOverlappingTreeTestClient()
	for i := 0; i < 2; i++ {
		result, err := listMCPRemoteTree(context.Background(), client, ListTreeArgs{Paths: []string{"A/B"}, MaxNodes: 10})
		if err != nil || result.Succeeded != 1 {
			t.Fatalf("list_tree call %d = %#v, %v", i, result, err)
		}
	}
	if got := client.listCalls["b:0:500"]; got != 2 {
		t.Fatalf("request-scoped snapshot leaked across independent tool calls: underlying calls=%d, want 2", got)
	}
}
