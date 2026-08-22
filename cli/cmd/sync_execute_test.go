package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func recordingSyncExecutionDeps(calls *[]string, failOn string) syncExecutionDeps {
	record := func(name string, item syncPlanItem) error {
		entry := name + ":" + item.RelativePath
		*calls = append(*calls, entry)
		if entry == failOn {
			return errors.New("planned failure")
		}
		return nil
	}
	return syncExecutionDeps{
		preflight:             func(context.Context) error { return nil },
		prepare:               func() error { return nil },
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error { return record("mkdir-remote", item) },
		removeRemote:          func(_ context.Context, item syncPlanItem) error { return record("remove-remote", item) },
		deleteRemote:          func(_ context.Context, item syncPlanItem) error { return record("delete-remote", item) },
		uploadFile:            func(_ context.Context, item syncPlanItem) error { return record("upload", item) },
		createLocalDirectory:  func(_ context.Context, item syncPlanItem) error { return record("mkdir-local", item) },
		removeLocal:           func(_ context.Context, item syncPlanItem) error { return record("remove-local", item) },
		deleteLocal:           func(_ context.Context, item syncPlanItem) error { return record("delete-local", item) },
		downloadFile:          func(_ context.Context, item syncPlanItem) error { return record("download", item) },
	}
}

func TestExecuteSyncPlanRunsNonDestructiveActionsInPlanOrder(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionBoth, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "a", Action: "upload", Kind: "directory"},
			{RelativePath: "a/file.bin", Action: "upload", Kind: "file"},
			{RelativePath: "b", Action: "download", Kind: "directory"},
			{RelativePath: "b/file.bin", Action: "download", Kind: "file"},
			{RelativePath: "same.bin", Action: "skip", Kind: "file"},
		},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, false, recordingSyncExecutionDeps(&calls, ""))
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"mkdir-remote:a", "upload:a/file.bin", "mkdir-local:b", "download:b/file.bin"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("execution order: got %#v want %#v", calls, wantCalls)
	}
	if summary.Processed != 5 || summary.Succeeded != 4 || summary.Skipped != 1 || summary.Failed != 0 {
		t.Fatalf("unexpected execution summary: %#v", summary)
	}
	if summary.UploadedFiles != 1 || summary.CreatedRemoteDirs != 1 || summary.DownloadedFiles != 1 || summary.CreatedLocalDirs != 1 {
		t.Fatalf("unexpected transfer counts: %#v", summary)
	}
}

func TestExecuteSyncPlanSkipOnlyNeedsNoExecutors(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionBoth, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "same.bin", Action: "skip", Kind: "file"}},
	}
	summary, err := executeSyncPlan(context.Background(), plan, false, syncExecutionDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Processed != 1 || summary.Skipped != 1 || summary.Succeeded != 0 || summary.Failed != 0 {
		t.Fatalf("unexpected skip-only summary: %#v", summary)
	}
}

func TestExecuteSyncPlanForcePreflightRunsForAllSkipResidual(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "already.bin", Action: "skip", Kind: "file"}},
	}
	calls := 0
	deps := syncExecutionDeps{
		forcePreflight: true,
		preflight: func(context.Context) error {
			calls++
			return errors.New("completed postcondition drifted")
		},
	}
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("forced all-skip preflight failure was ignored: %v", err)
	}
	if calls != 1 || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("all-skip resume crossed forced preflight barrier: calls=%d summary=%#v", calls, summary)
	}
}

func TestSyncPlanFileTransferNeedsIgnoresDirectoryOnlyActions(t *testing.T) {
	tests := []struct {
		name         string
		items        []syncPlanItem
		wantUpload   bool
		wantDownload bool
	}{
		{name: "skip-only", items: []syncPlanItem{{Action: "skip", Kind: "file"}}},
		{name: "directory-actions", items: []syncPlanItem{{Action: "upload", Kind: "directory"}, {Action: "download", Kind: "directory"}, {Action: "replace-remote", Kind: "directory"}, {Action: "replace-local", Kind: "directory"}, {Action: "delete-remote", Kind: "directory"}, {Action: "delete-local", Kind: "directory"}}},
		{name: "upload-file", items: []syncPlanItem{{Action: "upload", Kind: "file"}}, wantUpload: true},
		{name: "download-file", items: []syncPlanItem{{Action: "download", Kind: "file"}}, wantDownload: true},
		{name: "replacement-files", items: []syncPlanItem{{Action: "replace-remote", Kind: "file"}, {Action: "replace-local", Kind: "file"}}, wantUpload: true, wantDownload: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upload, download := syncPlanFileTransferNeeds(syncPlan{Items: test.items})
			if upload != test.wantUpload || download != test.wantDownload {
				t.Fatalf("transfer needs: got upload=%v download=%v want upload=%v download=%v", upload, download, test.wantUpload, test.wantDownload)
			}
		})
	}
}

func TestExecuteSyncPlanPreflightAndPrepareRunBeforeWrites(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "file.bin", Action: "upload", Kind: "file"}},
	}
	var calls []string
	deps := recordingSyncExecutionDeps(&calls, "")
	deps.preflight = func(context.Context) error {
		calls = append(calls, "preflight")
		return nil
	}
	deps.prepare = func() error {
		calls = append(calls, "prepare")
		return nil
	}
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preflight", "prepare", "upload:file.bin"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("execution phases: got %#v want %#v", calls, want)
	}
	if !summary.PreflightPassed || summary.PreflightChecked != 1 || summary.Processed != 1 {
		t.Fatalf("unexpected preflight summary: %#v", summary)
	}
}

func TestExecuteSyncPlanPreflightFailurePreventsPrepareAndWrites(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "file.bin", Action: "upload", Kind: "file"}},
	}
	var calls []string
	deps := recordingSyncExecutionDeps(&calls, "")
	deps.preflight = func(context.Context) error {
		calls = append(calls, "preflight")
		return errors.New("stale plan")
	}
	deps.prepare = func() error {
		calls = append(calls, "prepare")
		return nil
	}
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("expected preflight failure, got %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"preflight"}) || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("preflight failure crossed the write barrier: calls=%#v summary=%#v", calls, summary)
	}
}

func TestExecuteSyncPlanPreparationFailurePreventsWritesAfterPreflight(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "file.bin", Action: "upload", Kind: "file"}},
	}
	var calls []string
	deps := recordingSyncExecutionDeps(&calls, "")
	deps.preflight = func(context.Context) error {
		calls = append(calls, "preflight")
		return nil
	}
	prepareErr := errors.New("bad transfer config")
	deps.prepare = func() error {
		calls = append(calls, "prepare")
		return prepareErr
	}
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if !errors.Is(err, errSyncExecutionPreparation) || !errors.Is(err, prepareErr) {
		t.Fatalf("expected preparation sentinel and cause, got %v", err)
	}
	if got := syncExecutionErrorCode(err); got != output.ExitArgs {
		t.Fatalf("local preparation error exit = %d, want ExitArgs", got)
	}
	if !reflect.DeepEqual(calls, []string{"preflight", "prepare"}) || summary.Processed != 0 || !summary.PreflightPassed || summary.PreflightChecked != 1 {
		t.Fatalf("preparation failure crossed the write barrier: calls=%#v summary=%#v", calls, summary)
	}
}

func TestSyncExecutionPreparationNetworkFailureKeepsNetworkExit(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{{RelativePath: "file.bin", Action: "upload", Kind: "file"}},
	}
	deps := recordingSyncExecutionDeps(&[]string{}, "")
	networkErr := &net.DNSError{Err: "offline", Name: "proapi.115.com", IsTemporary: true}
	deps.prepare = func() error { return networkErr }

	_, err := executeSyncPlan(context.Background(), plan, false, deps)
	var dnsErr *net.DNSError
	if !errors.Is(err, errSyncExecutionPreparation) || !errors.As(err, &dnsErr) {
		t.Fatalf("preparation network error lost sentinel/cause: %v", err)
	}
	if got := syncExecutionErrorCode(err); got != output.ExitNetwork {
		t.Fatalf("preparation network exit = %d, want ExitNetwork", got)
	}
	if got := syncJournalExitCode(err); got != output.ExitNetwork {
		t.Fatalf("journal preparation network exit = %d, want ExitNetwork", got)
	}
}

func TestSyncExecutionSummaryJSONIncludesPreflightState(t *testing.T) {
	summary := newSyncExecutionSummaryWithJobs(syncPlan{PlanID: "plan-123", Items: []syncPlanItem{{}, {}}}, false, 4)
	summary.PreflightChecked = 2
	summary.PreflightPassed = true
	summary.JournalEnabled = true
	summary.JournalResumed = true
	summary.JournalCompletedBefore = 1
	summary.JournalState = "completed"
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"plan_id":"plan-123"`) || !strings.Contains(text, `"jobs":4`) || !strings.Contains(text, `"preflight_checked":2`) || !strings.Contains(text, `"preflight_passed":true`) || !strings.Contains(text, `"journal_enabled":true`) || !strings.Contains(text, `"journal_resumed":true`) || !strings.Contains(text, `"journal_completed_before":1`) || !strings.Contains(text, `"journal_state":"completed"`) {
		t.Fatalf("parallel/preflight fields missing from JSON summary: %s", text)
	}
}

func TestResolveAndValidateSyncDeleteBudget(t *testing.T) {
	if _, err := resolveSyncDeleteBudget(-1, ""); err == nil {
		t.Fatal("negative delete-root budget was accepted")
	}
	if _, err := resolveSyncDeleteBudgetWithItems(0, -1, ""); err == nil {
		t.Fatal("negative delete-item budget was accepted")
	}
	if _, err := resolveSyncDeleteBudget(0, "not-a-size"); err == nil {
		t.Fatal("invalid delete-byte budget was accepted")
	}
	if err := validateSyncDeleteBudgetUsage(false, syncDeleteBudget{MaxRoots: 1}); err == nil || !strings.Contains(err.Error(), "require --delete") {
		t.Fatalf("delete-root budget without --delete was accepted: %v", err)
	}
	if err := validateSyncDeleteBudgetUsage(false, syncDeleteBudget{MaxItems: 1}); err == nil || !strings.Contains(err.Error(), "require --delete") {
		t.Fatalf("delete-item budget without --delete was accepted: %v", err)
	}
	if err := validateSyncDeleteBudgetUsage(false, syncDeleteBudget{MaxBytesSet: true}); err == nil || !strings.Contains(err.Error(), "require --delete") {
		t.Fatalf("delete-byte budget without --delete was accepted: %v", err)
	}
	if err := validateSyncDeleteBudgetUsage(true, syncDeleteBudget{MaxRoots: 1, MaxItems: 1, MaxBytesSet: true}); err != nil {
		t.Fatalf("delete budgets with --delete were rejected: %v", err)
	}
	budget, err := resolveSyncDeleteBudgetWithItems(2, 3, "9B")
	if err != nil {
		t.Fatal(err)
	}
	plan := syncPlan{DeleteRemoteRoots: 2, DeleteRemoteFiles: 2, DeleteRemoteDirs: 1, DeleteRemoteBytes: 9}
	if err := validateSyncDeleteBudget(plan, budget); err != nil {
		t.Fatalf("exact delete budget rejected: %v", err)
	}
	if err := validateSyncDeleteBudget(syncPlan{DeleteRemoteRoots: 3, DeleteRemoteFiles: 2, DeleteRemoteDirs: 1, DeleteRemoteBytes: 9}, budget); err == nil || !strings.Contains(err.Error(), "root budget exceeded") {
		t.Fatalf("delete-root overrun was accepted: %v", err)
	}
	if err := validateSyncDeleteBudget(syncPlan{DeleteRemoteRoots: 2, DeleteRemoteFiles: 3, DeleteRemoteDirs: 1, DeleteRemoteBytes: 9}, budget); err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("delete-item overrun was accepted: %v", err)
	}
	if err := validateSyncDeleteBudget(syncPlan{DeleteRemoteFiles: 1, DeleteRemoteBytes: 10}, budget); err == nil || !strings.Contains(err.Error(), "byte budget exceeded") {
		t.Fatalf("delete-byte overrun was accepted: %v", err)
	}
	unlimited, err := resolveSyncDeleteBudget(0, "")
	if err != nil || validateSyncDeleteBudget(syncPlan{DeleteRemoteRoots: 1000, DeleteRemoteFiles: 1000, DeleteRemoteBytes: 1 << 40}, unlimited) != nil {
		t.Fatalf("unlimited delete budget did not stay unlimited: budget=%#v err=%v", unlimited, err)
	}
}

func TestExecuteSyncPlanWithJobsRunsIndependentFilesConcurrentlyAndOrdersSummary(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "first.bin", Action: "upload", Kind: "file"},
			{RelativePath: "second.bin", Action: "upload", Kind: "file"},
		},
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		prepare:   func() error { return nil },
		uploadFile: func(_ context.Context, item syncPlanItem) error {
			started <- item.RelativePath
			<-release
			return nil
		},
	}
	type executionResult struct {
		summary syncExecutionSummary
		err     error
	}
	done := make(chan executionResult, 1)
	go func() {
		summary, err := executeSyncPlanWithJobs(context.Background(), plan, false, 2, deps)
		done <- executionResult{summary: summary, err: err}
	}()
	seen := make(map[string]bool)
	for range 2 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(time.Second):
			t.Fatal("two independent sync files did not start concurrently")
		}
	}
	if !seen["first.bin"] || !seen["second.bin"] {
		t.Fatalf("unexpected concurrent starts: %#v", seen)
	}
	close(release)
	released = true
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.summary.Jobs != 2 || result.summary.Processed != 2 || result.summary.Succeeded != 2 || len(result.summary.Items) != 2 {
		t.Fatalf("unexpected parallel summary: %#v", result.summary)
	}
	if result.summary.Items[0].RelativePath != "first.bin" || result.summary.Items[1].RelativePath != "second.bin" {
		t.Fatalf("parallel summary order is not deterministic: %#v", result.summary.Items)
	}
}

func TestRunSyncPlanItemAcquiresFileSlotBeforeDestructiveReplacement(t *testing.T) {
	item := syncPlanItem{RelativePath: "replace.bin", Action: "replace-remote", Kind: "file"}
	var events []string
	deps := syncExecutionDeps{
		acquireFileTransfer: func(context.Context) (func(), error) {
			events = append(events, "acquire")
			return func() { events = append(events, "release") }, nil
		},
		removeRemote: func(context.Context, syncPlanItem) error {
			events = append(events, "remove")
			return nil
		},
		uploadFile: func(context.Context, syncPlanItem) error {
			events = append(events, "upload")
			return nil
		},
	}
	outcome := runSyncPlanItem(context.Background(), 0, item, deps)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	want := []string{"acquire", "remove", "upload", "release"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("file slot ordering: got %#v want %#v", events, want)
	}
}

func TestExecuteSyncPlanWithJobsWaitsForDirectoryBarrierBeforeChild(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "dir", Action: "upload", Kind: "directory"},
			{RelativePath: "dir/child.bin", Action: "upload", Kind: "file"},
			{RelativePath: "other.bin", Action: "upload", Kind: "file"},
		},
	}
	events := make(chan string, 3)
	releaseParent := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseParent)
		}
	}()
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		prepare:   func() error { return nil },
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error {
			events <- "mkdir:" + item.RelativePath
			<-releaseParent
			return nil
		},
		uploadFile: func(_ context.Context, item syncPlanItem) error {
			events <- "upload:" + item.RelativePath
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := executeSyncPlanWithJobs(context.Background(), plan, false, 2, deps)
		done <- err
	}()
	firstWave := make(map[string]bool)
	for range 2 {
		select {
		case event := <-events:
			firstWave[event] = true
		case <-time.After(time.Second):
			t.Fatal("first sync execution wave did not start")
		}
	}
	if !firstWave["mkdir:dir"] || !firstWave["upload:other.bin"] || firstWave["upload:dir/child.bin"] {
		t.Fatalf("directory barrier did not isolate child work: %#v", firstWave)
	}
	close(releaseParent)
	released = true
	select {
	case event := <-events:
		if event != "upload:dir/child.bin" {
			t.Fatalf("unexpected post-barrier action: %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("directory child did not run after parent barrier")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteSyncPlanWithJobsStopsBeforeNextWaveAfterFailure(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "first.bin", Action: "upload", Kind: "file"},
			{RelativePath: "second.bin", Action: "upload", Kind: "file"},
			{RelativePath: "third.bin", Action: "upload", Kind: "file"},
		},
	}
	started := make(chan string, 3)
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		prepare:   func() error { return nil },
		uploadFile: func(_ context.Context, item syncPlanItem) error {
			started <- item.RelativePath
			if item.RelativePath == "second.bin" {
				return errors.New("parallel wave failure")
			}
			return nil
		},
	}
	summary, err := executeSyncPlanWithJobs(context.Background(), plan, false, 2, deps)
	if err == nil || !strings.Contains(err.Error(), "second.bin") {
		t.Fatalf("expected second item failure, got %v", err)
	}
	close(started)
	var paths []string
	for path := range started {
		paths = append(paths, path)
	}
	if len(paths) != 2 {
		t.Fatalf("failure wave launched later work: %#v", paths)
	}
	for _, path := range paths {
		if path == "third.bin" {
			t.Fatalf("third item started after failed wave: %#v", paths)
		}
	}
	if summary.Jobs != 2 || summary.Processed != 2 || summary.Succeeded != 1 || summary.Failed != 1 || len(summary.Items) != 2 {
		t.Fatalf("unexpected failed-wave summary: %#v", summary)
	}
	if summary.Items[0].RelativePath != "first.bin" || summary.Items[1].RelativePath != "second.bin" || summary.Items[1].Status != "failed" {
		t.Fatalf("failed-wave summary order/status: %#v", summary.Items)
	}
}

func TestExecuteSyncPlanContinueOnErrorBlocksFailedBranchAndContinuesIndependentBranch(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "bad", Action: "upload", Kind: "directory"},
			{RelativePath: "bad/child.bin", Action: "upload", Kind: "file"},
			{RelativePath: "good", Action: "upload", Kind: "directory"},
			{RelativePath: "good/child.bin", Action: "upload", Kind: "file"},
		},
	}
	var (
		mu    sync.Mutex
		calls []string
	)
	record := func(value string) {
		mu.Lock()
		calls = append(calls, value)
		mu.Unlock()
	}
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		prepare:   func() error { return nil },
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error {
			record("mkdir:" + item.RelativePath)
			if item.RelativePath == "bad" {
				return errors.New("bad branch failed")
			}
			return nil
		},
		uploadFile: func(_ context.Context, item syncPlanItem) error {
			record("upload:" + item.RelativePath)
			return nil
		},
	}
	summary, err := executeSyncPlanWithJobsPolicy(context.Background(), plan, false, 2, true, deps)
	if err == nil || !strings.Contains(err.Error(), "1 failed action(s)") || !strings.Contains(err.Error(), "1 blocked dependent item(s)") {
		t.Fatalf("continue-on-error did not return aggregate failure: %v", err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	for _, call := range gotCalls {
		if call == "upload:bad/child.bin" {
			t.Fatalf("blocked descendant executed: %#v", gotCalls)
		}
	}
	seenGoodChild := false
	for _, call := range gotCalls {
		if call == "upload:good/child.bin" {
			seenGoodChild = true
		}
	}
	if !seenGoodChild {
		t.Fatalf("independent branch did not continue: %#v", gotCalls)
	}
	if !summary.ContinueOnError || summary.Processed != 3 || summary.Succeeded != 2 || summary.Failed != 1 || summary.Blocked != 1 || len(summary.Items) != 4 {
		t.Fatalf("unexpected continue-on-error summary: %#v", summary)
	}
	wantStatuses := []string{"failed", "blocked", "succeeded", "succeeded"}
	for index, want := range wantStatuses {
		if summary.Items[index].RelativePath != plan.Items[index].RelativePath || summary.Items[index].Status != want {
			t.Fatalf("summary item %d: got %#v want path=%q status=%q", index, summary.Items[index], plan.Items[index].RelativePath, want)
		}
	}
	if !strings.Contains(summary.Items[1].Error, "bad") {
		t.Fatalf("blocked item does not identify failed dependency: %#v", summary.Items[1])
	}
}

func TestValidateSyncFailurePolicyRequiresContinueOnErrorForMaxErrors(t *testing.T) {
	if err := validateSyncFailurePolicy(false, -1); err == nil || !strings.Contains(err.Error(), "must be >= 0") {
		t.Fatalf("negative max-errors was accepted: %v", err)
	}
	if err := validateSyncFailurePolicy(false, 1); err == nil || !strings.Contains(err.Error(), "requires --continue-on-error") {
		t.Fatalf("max-errors without continue-on-error was accepted: %v", err)
	}
	if err := validateSyncFailurePolicy(true, 1); err != nil {
		t.Fatalf("valid max-errors policy rejected: %v", err)
	}
}

func TestExecuteSyncPlanContinueOnErrorStopsBeforeNextWaveAtMaxErrors(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "first.bin", Action: "upload", Kind: "file"},
			{RelativePath: "second.bin", Action: "upload", Kind: "file"},
			{RelativePath: "third.bin", Action: "upload", Kind: "file"},
		},
	}
	var calls []string
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		prepare:   func() error { return nil },
		uploadFile: func(_ context.Context, item syncPlanItem) error {
			calls = append(calls, item.RelativePath)
			return errors.New("intentional failure")
		},
	}
	summary, err := executeSyncPlanWithJobsFailurePolicy(context.Background(), plan, false, 1, true, 1, deps)
	if err == nil || !strings.Contains(err.Error(), "reaching --max-errors 1") {
		t.Fatalf("max-errors did not stop execution: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"first.bin"}) {
		t.Fatalf("later wave started after max-errors: %#v", calls)
	}
	if !summary.ContinueOnError || summary.MaxErrors != 1 || summary.Processed != 1 || summary.Failed != 1 || summary.Blocked != 0 || len(summary.Items) != 1 {
		t.Fatalf("unexpected max-errors summary: %#v", summary)
	}
}

func TestExecuteSyncPlanRejectsMaxErrorsPolicyBeforePreflight(t *testing.T) {
	plan := syncPlan{Ready: true, Items: []syncPlanItem{{RelativePath: "file.bin", Action: "upload", Kind: "file"}}}
	var calls []string
	deps := recordingSyncExecutionDeps(&calls, "")
	summary, err := executeSyncPlanWithJobsFailurePolicy(context.Background(), plan, false, 1, false, 1, deps)
	if err == nil || !strings.Contains(err.Error(), "requires --continue-on-error") {
		t.Fatalf("invalid max-errors execution policy was accepted: %v", err)
	}
	if len(calls) != 0 || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("invalid max-errors policy crossed preflight barrier: calls=%#v summary=%#v", calls, summary)
	}
}

func TestBlockSyncExecutionDependentsBlocksTransitiveSubtreeOnce(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "root", Action: "upload", Kind: "directory"},
		{RelativePath: "root/sub", Action: "upload", Kind: "directory"},
		{RelativePath: "root/sub/file.bin", Action: "upload", Kind: "file"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	blocked := make([]bool, len(plan.Items))
	outcomes := make([]*syncExecutionItemOutcome, len(plan.Items))
	if got := blockSyncExecutionDependents(0, "root", plan, graph, blocked, outcomes); got != 2 {
		t.Fatalf("blocked %d dependent(s), want 2", got)
	}
	if got := blockSyncExecutionDependents(0, "root", plan, graph, blocked, outcomes); got != 0 {
		t.Fatalf("reblocking marked %d dependent(s), want 0", got)
	}
	for _, index := range []int{1, 2} {
		if outcomes[index] == nil || outcomes[index].Result.Status != "blocked" {
			t.Fatalf("dependent %d was not blocked: %#v", index, outcomes[index])
		}
	}
}

func TestExecuteSyncPlanDestructiveGateRunsBeforeAnyExecutor(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionBoth, ConflictPolicy: syncConflictLocal, DestructiveActions: 1,
		Items: []syncPlanItem{{RelativePath: "replace.bin", Action: "replace-remote", Kind: "file", Destructive: true}},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, false, recordingSyncExecutionDeps(&calls, ""))
	if !errors.Is(err, errSyncDestructiveApproval) {
		t.Fatalf("expected destructive approval error, got %v", err)
	}
	if len(calls) != 0 || summary.Processed != 0 {
		t.Fatalf("destructive gate ran executors: calls=%#v summary=%#v", calls, summary)
	}
}

func TestExecuteSyncPlanMirrorDeletesRequireDestructiveApproval(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError, DeleteExtraneous: true, DestructiveActions: 1,
		Items: []syncPlanItem{{RelativePath: "orphan.bin", Action: "delete-remote", Kind: "file", Destructive: true}},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, false, recordingSyncExecutionDeps(&calls, ""))
	if !errors.Is(err, errSyncDestructiveApproval) {
		t.Fatalf("expected mirror-delete approval error, got %v", err)
	}
	if len(calls) != 0 || summary.Processed != 0 || !summary.DeleteExtraneous {
		t.Fatalf("mirror-delete gate ran executors: calls=%#v summary=%#v", calls, summary)
	}
}

func TestExecuteSyncPlanRunsCollapsedMirrorDeleteRoots(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError, DeleteExtraneous: true, DestructiveActions: 2,
		Items: []syncPlanItem{
			{RelativePath: "old-dir", Action: "delete-remote", Kind: "directory", Destructive: true},
			{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", Destructive: true},
			{RelativePath: "old-dir/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-remote:old-dir"},
		},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, true, recordingSyncExecutionDeps(&calls, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delete-remote:old-dir", "delete-remote:old.bin"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("mirror-delete execution duplicated covered descendants: got %#v want %#v", calls, want)
	}
	if summary.DeletedRemote != 2 || summary.DeletedLocal != 0 || summary.Succeeded != 2 || summary.Skipped != 1 || !summary.DeleteExtraneous {
		t.Fatalf("unexpected mirror-delete summary: %#v", summary)
	}
}

func TestRunSyncPlanItemMirrorDeleteDoesNotAcquireFileTransferSlot(t *testing.T) {
	var acquired bool
	item := syncPlanItem{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", Destructive: true}
	deps := syncExecutionDeps{
		acquireFileTransfer: func(context.Context) (func(), error) {
			acquired = true
			return func() {}, nil
		},
		deleteRemote: func(context.Context, syncPlanItem) error { return nil },
	}
	outcome := runSyncPlanItem(context.Background(), 0, item, deps)
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	if acquired || outcome.DeletedRemote != 1 {
		t.Fatalf("mirror delete consumed transfer capacity: acquired=%v outcome=%#v", acquired, outcome)
	}
}

func TestExecuteSyncPlanRejectsUnresolvedConflictBeforeWrites(t *testing.T) {
	plan := syncPlan{
		Ready: false, Direction: syncDirectionBoth, ConflictPolicy: syncConflictError, Conflicts: 1,
		Items: []syncPlanItem{{RelativePath: "conflict.bin", Action: "conflict", Kind: "file"}},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, true, recordingSyncExecutionDeps(&calls, ""))
	if !errors.Is(err, errSyncPlanNotReady) {
		t.Fatalf("expected unresolved conflict error, got %v", err)
	}
	if len(calls) != 0 || summary.Processed != 0 {
		t.Fatalf("unresolved plan ran executors: calls=%#v summary=%#v", calls, summary)
	}
}

func TestExecuteSyncPlanReplacementRemovesLoserBeforeWinner(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionBoth, ConflictPolicy: syncConflictLocal, DestructiveActions: 2,
		Items: []syncPlanItem{
			{RelativePath: "remote-dir", Action: "replace-remote", Kind: "directory", Destructive: true},
			{RelativePath: "local-file", Action: "replace-local", Kind: "file", Destructive: true},
		},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, true, recordingSyncExecutionDeps(&calls, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"remove-remote:remote-dir", "mkdir-remote:remote-dir", "remove-local:local-file", "download:local-file"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("replacement order: got %#v want %#v", calls, want)
	}
	if summary.ReplacedRemote != 1 || summary.ReplacedLocal != 1 || summary.CreatedRemoteDirs != 1 || summary.DownloadedFiles != 1 {
		t.Fatalf("unexpected replacement summary: %#v", summary)
	}
}

func TestExecuteSyncPlanStopsOnFirstFailureAndReturnsPartialSummary(t *testing.T) {
	plan := syncPlan{
		Ready: true, Direction: syncDirectionBoth, ConflictPolicy: syncConflictError,
		Items: []syncPlanItem{
			{RelativePath: "first.bin", Action: "upload", Kind: "file"},
			{RelativePath: "second.bin", Action: "download", Kind: "file"},
			{RelativePath: "third.bin", Action: "upload", Kind: "file"},
		},
	}
	var calls []string
	summary, err := executeSyncPlan(context.Background(), plan, false, recordingSyncExecutionDeps(&calls, "download:second.bin"))
	if err == nil || !strings.Contains(err.Error(), "second.bin") {
		t.Fatalf("expected second action failure, got %v", err)
	}
	want := []string{"upload:first.bin", "download:second.bin"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("executor continued after failure: %#v", calls)
	}
	if summary.Processed != 2 || summary.Succeeded != 1 || summary.Failed != 1 || len(summary.Items) != 2 || summary.Items[1].Status != "failed" {
		t.Fatalf("unexpected partial summary: %#v", summary)
	}
}

func TestExecuteSyncPlanWholePreflightRejectsLaterStaleItemBeforeFirstWrite(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "a.bin", "aaaa")
	writeSyncTestFile(t, localRoot, "z.bin", "zzzz")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	zPath := filepath.Join(localRoot, "z.bin")
	info, err := os.Lstat(zPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(zPath, changed, changed); err != nil {
		t.Fatal(err)
	}
	var calls []string
	deps := recordingSyncExecutionDeps(&calls, "")
	deps.preflight = func(ctx context.Context) error { return preflightSyncPlan(ctx, client, plan) }
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if err == nil || !strings.Contains(err.Error(), "z.bin") {
		t.Fatalf("later stale item did not fail whole-plan preflight: %v", err)
	}
	if len(calls) != 0 || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("first write occurred before later stale item was detected: calls=%#v summary=%#v", calls, summary)
	}
}

func TestPreflightSyncPlanAcceptsNestedPlannedCreates(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "nested/child.bin", "child")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightSyncPlan(context.Background(), client, plan); err != nil {
		t.Fatalf("unchanged nested-create plan failed preflight: %v", err)
	}
}

func TestPreflightSyncPlanRejectsLaterLocalMutation(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "a.bin", "aaaa")
	writeSyncTestFile(t, localRoot, "z.bin", "zzzz")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, localRoot, "z.bin", "ZZZZ")
	zPath := filepath.Join(localRoot, "z.bin")
	info, err := os.Lstat(zPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(zPath, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := preflightSyncPlan(context.Background(), client, plan); err == nil || !strings.Contains(err.Error(), "z.bin") {
		t.Fatalf("later local mutation was not rejected before execution: %v", err)
	}
}

func TestPreflightSyncPlanRejectsEntryAppearingOnEitherSide(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "local.bin", "data")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	client.lists["root"] = []driver.File{{FileID: "surprise", Name: "surprise.bin", Size: 1, Sha1: testSyncSHA1("x")}}
	if err := preflightSyncPlan(context.Background(), client, plan); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("remote entry appearing after planning was not rejected: %v", err)
	}
	client.lists["root"] = nil
	writeSyncTestFile(t, localRoot, "surprise-local.bin", "x")
	if err := preflightSyncPlan(context.Background(), client, plan); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("local entry appearing after planning was not rejected: %v", err)
	}
}

func TestPreflightSyncPlanRejectsRemoteRootIdentityChange(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "file.bin", "data")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	client.dirIDs["remote"] = "replacement-root"
	client.lists["replacement-root"] = nil
	if err := preflightSyncPlan(context.Background(), client, plan); err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("remote root identity change was not rejected: %v", err)
	}
}

func TestSyncValidateLocalSnapshotDetectsSameSizeChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.bin")
	if err := os.WriteFile(path, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	item := syncPlanItem{LocalPath: path, LocalSize: 4, LocalModTimeUnixNano: info.ModTime().UnixNano()}
	if err := syncValidateLocalSnapshot(item, "file"); err != nil {
		t.Fatalf("unchanged snapshot rejected: %v", err)
	}
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := syncValidateLocalSnapshot(item, "file"); err == nil || !strings.Contains(err.Error(), "modification time") {
		t.Fatalf("same-size local mutation was not rejected: %v", err)
	}
}

func TestSyncExecutionPathContainmentRejectsRootsAndEscapes(t *testing.T) {
	root := t.TempDir()
	if err := ensureSyncLocalPathWithinRoot(root, filepath.Join(root, "child")); err != nil {
		t.Fatalf("valid local child rejected: %v", err)
	}
	if err := ensureSyncLocalPathWithinRoot(root, root); err == nil {
		t.Fatal("local sync root itself was accepted as a mutable target")
	}
	if err := ensureSyncLocalPathWithinRoot(root, filepath.Join(filepath.Dir(root), "outside")); err == nil {
		t.Fatal("local path outside sync root was accepted")
	}
	if err := ensureSyncRemotePathWithinRoot("/remote", "/remote/child"); err != nil {
		t.Fatalf("valid remote child rejected: %v", err)
	}
	if err := ensureSyncRemotePathWithinRoot("/remote", "/remote"); err == nil {
		t.Fatal("remote sync root itself was accepted as a mutable target")
	}
	if err := ensureSyncRemotePathWithinRoot("/remote", "/other/child"); err == nil {
		t.Fatal("remote path outside sync root was accepted")
	}
}

func TestSyncEnsureRemoteAbsentRejectsPathThatAppearedAfterPlanning(t *testing.T) {
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "new", Name: "new.bin", Size: 1}},
		},
		files: map[string]driver.File{},
	}
	if err := syncEnsureRemoteAbsent(client, "/remote/missing.bin"); err != nil {
		t.Fatalf("missing path rejected: %v", err)
	}
	if err := syncEnsureRemoteAbsent(client, "/remote/new.bin"); err == nil || !strings.Contains(err.Error(), "appeared after planning") {
		t.Fatalf("concurrent remote creation was not rejected: %v", err)
	}
}

func TestSyncValidateLocalReplacementSubtreeRejectsDeepNewEntry(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "node/sub/old.bin", "old")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote-node", Name: "node", Size: 4, Sha1: testSyncSHA1("file")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictRemote})
	if err != nil {
		t.Fatal(err)
	}
	rootItem := syncActionsByPath(plan)["node"]
	if rootItem.Action != "replace-local" || rootItem.ReplacesKind != "directory" {
		t.Fatalf("unexpected replacement root: %#v", rootItem)
	}
	if err := syncValidateLocalReplacementSubtree(plan, rootItem); err != nil {
		t.Fatalf("unchanged local subtree rejected: %v", err)
	}
	writeSyncTestFile(t, localRoot, "node/sub/new.bin", "new")
	if err := syncValidateLocalReplacementSubtree(plan, rootItem); err == nil {
		t.Fatal("deep local subtree mutation was not rejected")
	}
}

func TestSyncProductionDeleteLocalDirectoryRevalidatesWholeSubtreeBeforeRemoveAll(t *testing.T) {
	buildDeletePlan := func(t *testing.T) (string, syncPlan, syncPlanItem) {
		t.Helper()
		localRoot := t.TempDir()
		writeSyncTestFile(t, localRoot, "orphan/sub/old.bin", "old")
		client := &syncReadOnlyClient{
			dirIDs: map[string]string{"remote": "root"},
			lists:  map[string][]driver.File{"root": {}},
			files:  map[string]driver.File{},
		}
		plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionDownload, ConflictPolicy: syncConflictError, DeleteExtraneous: true})
		if err != nil {
			t.Fatal(err)
		}
		item := syncActionsByPath(plan)["orphan"]
		if item.Action != "delete-local" || item.Kind != "directory" {
			t.Fatalf("unexpected local mirror-delete root: %#v", item)
		}
		return localRoot, plan, item
	}

	t.Run("unchanged-subtree-is-removed", func(t *testing.T) {
		_, plan, item := buildDeletePlan(t)
		executor := &syncProductionExecutor{plan: plan}
		if err := executor.deleteLocal(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(item.LocalPath); !os.IsNotExist(err) {
			t.Fatalf("planned local mirror-delete root still exists: err=%v", err)
		}
	})

	t.Run("new-descendant-makes-plan-stale", func(t *testing.T) {
		localRoot, plan, item := buildDeletePlan(t)
		writeSyncTestFile(t, localRoot, "orphan/sub/new.bin", "new")
		executor := &syncProductionExecutor{plan: plan}
		if err := executor.deleteLocal(context.Background(), item); err == nil {
			t.Fatal("stale local mirror-delete subtree was removed")
		}
		for _, relative := range []string{"orphan/sub/old.bin", "orphan/sub/new.bin"} {
			if _, err := os.Lstat(filepath.Join(localRoot, filepath.FromSlash(relative))); err != nil {
				t.Fatalf("stale mirror-delete removed %q despite rejection: %v", relative, err)
			}
		}
	})
}

func TestSyncValidateRemoteReplacementSubtreeRejectsDeepNewEntry(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "node", "local-file")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root":        {{FileID: "remote-node", Name: "node", IsDirectory: true}},
			"remote-node": {{FileID: "sub", Name: "sub", IsDirectory: true}},
			"sub":         {{FileID: "old", Name: "old.bin", Size: 3, Sha1: testSyncSHA1("old")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	rootItem := syncActionsByPath(plan)["node"]
	if rootItem.Action != "replace-remote" || rootItem.ReplacesKind != "directory" {
		t.Fatalf("unexpected replacement root: %#v", rootItem)
	}
	if err := syncValidateRemoteReplacementSubtree(client, plan, rootItem); err != nil {
		t.Fatalf("unchanged remote subtree rejected: %v", err)
	}
	client.lists["sub"] = append(client.lists["sub"], driver.File{FileID: "new", Name: "new.bin", Size: 3, Sha1: testSyncSHA1("new")})
	if err := syncValidateRemoteReplacementSubtree(client, plan, rootItem); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
		t.Fatalf("deep remote subtree mutation was not rejected: %v", err)
	}
}

func TestSyncPreparedDigestSurvivesPlanForExecutionReuse(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "conflict.bin", "aaaa")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote", Name: "conflict.bin", Size: 4, Sha1: testSyncSHA1("bbbb")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["conflict.bin"]
	if item.Action != "replace-remote" || item.LocalPreparedDigest == nil {
		t.Fatalf("planner did not retain prepared digest for execution: %#v", item)
	}
	if item.LocalPreparedDigest.SHA1 != item.LocalSHA1 || item.LocalPreparedDigest.Size != item.LocalSize || item.LocalPreparedDigest.ModTimeUnixNano != item.LocalModTimeUnixNano {
		t.Fatalf("prepared digest does not match planned local snapshot: item=%#v digest=%#v", item, item.LocalPreparedDigest)
	}
}

func TestSyncValidateRemoteSnapshotRejectsChangedIdentity(t *testing.T) {
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "new-id", Name: "file.bin", Size: 3, Sha1: testSyncSHA1("abc")}},
		},
		files: map[string]driver.File{
			"new-id": {FileID: "new-id", Name: "file.bin", Size: 3, Sha1: testSyncSHA1("abc")},
		},
	}
	item := syncPlanItem{RemotePath: "/remote/file.bin", RemoteID: "old-id", RemoteSize: 3, RemoteSHA1: testSyncSHA1("abc")}
	if err := syncValidateRemoteSnapshot(client, item, "file"); err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("changed remote identity was not rejected: %v", err)
	}
}
