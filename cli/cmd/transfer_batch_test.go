package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

func TestValidateBatchUploadSourcesRejectsAmbiguousModesAndNameCollisions(t *testing.T) {
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	t.Cleanup(func() {
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
	})

	root := t.TempDir()
	leftDir := filepath.Join(root, "left")
	rightDir := filepath.Join(root, "right")
	if err := os.MkdirAll(leftDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rightDir, 0755); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(leftDir, "same.bin")
	right := filepath.Join(rightDir, "same.bin")
	if err := os.WriteFile(left, []byte("left"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte("right"), 0600); err != nil {
		t.Fatal(err)
	}

	uploadRecursive = false
	uploadContents = false
	uploadSession = ""
	if err := validateBatchUploadSources([]string{left, right}); err == nil || !strings.Contains(err.Error(), "same remote name") {
		t.Fatalf("expected basename collision, got %v", err)
	}

	uploadSession = filepath.Join(root, "manual.session.json")
	if err := validateBatchUploadSources([]string{left, filepath.Join(root, "other.bin")}); err == nil || !strings.Contains(err.Error(), "--session") {
		t.Fatalf("expected explicit session rejection, got %v", err)
	}
	uploadSession = ""
	uploadContents = true
	if err := validateBatchUploadSources([]string{left, right}); err == nil || !strings.Contains(err.Error(), "--contents") {
		t.Fatalf("expected contents-mode rejection, got %v", err)
	}
}

func TestValidateBatchUploadSourcesAcceptsDistinctFiles(t *testing.T) {
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	t.Cleanup(func() {
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
	})
	uploadRecursive = false
	uploadContents = false
	uploadSession = ""

	root := t.TempDir()
	paths := []string{filepath.Join(root, "a.bin"), filepath.Join(root, "b.bin")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateBatchUploadSources(paths); err != nil {
		t.Fatalf("distinct file batch rejected: %v", err)
	}
}

func TestPrepareBatchDownloadSourcesRejectsDuplicateBasenamesBeforeTransfer(t *testing.T) {
	oldRecursive := downloadRecursive
	t.Cleanup(func() { downloadRecursive = oldRecursive })
	downloadRecursive = false

	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"a": "da", "b": "db"},
		lists: map[string][]driver.File{
			"da": {{FileID: "fa", Name: "same.bin"}},
			"db": {{FileID: "fb", Name: "same.bin"}},
		},
	}
	_, err := prepareBatchDownloadSources(client, []string{"/a/same.bin", "/b/same.bin"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "same local path") {
		t.Fatalf("expected local basename collision, got %v", err)
	}
}

func TestPrepareBatchDownloadSourcesPreservesEachRemoteBasename(t *testing.T) {
	oldRecursive := downloadRecursive
	t.Cleanup(func() { downloadRecursive = oldRecursive })
	downloadRecursive = true

	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"folder": "df", "a": "da"},
		lists: map[string][]driver.File{
			"df": {},
			"da": {{FileID: "fa", Name: "file.bin"}},
		},
	}
	localDir := filepath.Join(t.TempDir(), "batch")
	plans, err := prepareBatchDownloadSources(client, []string{"/folder", "/a/file.bin"}, localDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("unexpected plans: %#v", plans)
	}
	if plans[0].LocalPath != filepath.Join(localDir, "folder") || plans[1].LocalPath != filepath.Join(localDir, "file.bin") {
		t.Fatalf("unexpected local targets: %#v", plans)
	}
	if info, err := os.Stat(localDir); err != nil || !info.IsDir() {
		t.Fatalf("batch destination was not created: info=%v err=%v", info, err)
	}
}

func TestRemoteBatchBaseNameRejectsRoot(t *testing.T) {
	if _, err := remoteBatchBaseName("/"); err == nil {
		t.Fatal("remote root was accepted as one source in a batch download")
	}
	if got, err := remoteBatchBaseName("/a/b/"); err != nil || got != "b" {
		t.Fatalf("unexpected basename: got=%q err=%v", got, err)
	}
}

func TestBatchUploadContinueOnErrorProcessesRemainingItemsAndReturnsStructuredFailure(t *testing.T) {
	oldRun := uploadSingleRunE
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		uploadSingleRunE = oldRun
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
		jsonOutput, printer = oldJSON, oldPrinter
	})
	uploadRecursive = false
	uploadContents = false
	uploadSession = ""
	jsonOutput = true
	printer = output.NewPrinter(false)

	root := t.TempDir()
	sources := []string{filepath.Join(root, "a.bin"), filepath.Join(root, "b.bin"), filepath.Join(root, "c.bin")}
	for _, source := range sources {
		if err := os.WriteFile(source, []byte(filepath.Base(source)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	uploadSingleRunE = func(_ *cobra.Command, args []string) error {
		calls = append(calls, args[0])
		if args[0] == sources[1] {
			return &exitError{code: output.ExitNetwork, msg: "simulated disconnect"}
		}
		return nil
	}
	cmd := &cobra.Command{}
	addContinueOnErrorFlag(cmd)
	if err := cmd.Flags().Set("continue-on-error", "true"); err != nil {
		t.Fatal(err)
	}
	err := runBatchUploadCommand(cmd, append(append([]string(nil), sources...), "/remote"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.data == nil {
		t.Fatalf("expected structured batch exit error, got %T %v", err, err)
	}
	if ee.code != output.ExitNetwork {
		t.Fatalf("batch exit code = %d, want ExitNetwork", ee.code)
	}
	if len(calls) != 3 {
		t.Fatalf("continue-on-error did not process all sources: %#v", calls)
	}
	data, ok := ee.data.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected batch data type %T", ee.data)
	}
	if data["processed"] != 3 || data["succeeded"] != 2 || data["failed"] != 1 || data["remaining"] != 0 {
		t.Fatalf("unexpected batch counters: %#v", data)
	}
	items, ok := data["items"].([]batchItemResult)
	if !ok || len(items) != 3 || items[1].Success || items[1].Code != output.ExitNetwork {
		t.Fatalf("unexpected batch items: %#v", data["items"])
	}
}

func TestBatchFailureExitCodeFallsBackToGenericForMixedFailures(t *testing.T) {
	items := []batchItemResult{
		{Success: false, Code: output.ExitNetwork},
		{Success: false, Code: output.ExitNotFound},
	}
	if got := batchFailureExitCode(items); got != output.ExitError {
		t.Fatalf("mixed batch exit code = %d, want ExitError", got)
	}
}

func TestBatchUploadFailFastStopsAfterFirstRuntimeFailure(t *testing.T) {
	oldRun := uploadSingleRunE
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		uploadSingleRunE = oldRun
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
		jsonOutput, printer = oldJSON, oldPrinter
	})
	uploadRecursive = false
	uploadContents = false
	uploadSession = ""
	jsonOutput = true
	printer = output.NewPrinter(false)

	root := t.TempDir()
	sources := []string{filepath.Join(root, "a.bin"), filepath.Join(root, "b.bin"), filepath.Join(root, "c.bin")}
	for _, source := range sources {
		if err := os.WriteFile(source, []byte(filepath.Base(source)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	uploadSingleRunE = func(_ *cobra.Command, args []string) error {
		calls = append(calls, args[0])
		if args[0] == sources[1] {
			return &exitError{code: output.ExitError, msg: "simulated failure"}
		}
		return nil
	}
	cmd := &cobra.Command{}
	addContinueOnErrorFlag(cmd)
	err := runBatchUploadCommand(cmd, append(append([]string(nil), sources...), "/remote"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.data == nil {
		t.Fatalf("expected structured fail-fast error, got %T %v", err, err)
	}
	if ee.code != output.ExitError {
		t.Fatalf("fail-fast batch exit code = %d, want ExitError", ee.code)
	}
	if len(calls) != 2 {
		t.Fatalf("fail-fast processed unexpected sources: %#v", calls)
	}
	data := ee.data.(map[string]interface{})
	if data["processed"] != 2 || data["succeeded"] != 1 || data["failed"] != 1 || data["remaining"] != 1 {
		t.Fatalf("unexpected fail-fast counters: %#v", data)
	}
}

func TestPrepareBatchDownloadPlansKeepsPerItemErrorsWithoutLosingValidTargets(t *testing.T) {
	oldRecursive := downloadRecursive
	t.Cleanup(func() { downloadRecursive = oldRecursive })
	downloadRecursive = false
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"ok": "dok"},
		lists: map[string][]driver.File{
			"dok": {{FileID: "f-ok", Name: "ok.bin"}},
		},
	}
	plans, err := prepareBatchDownloadPlans(client, []string{"/missing.bin", "/ok/ok.bin"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Err == nil || plans[1].Err != nil || plans[1].Source.RemotePath != "/ok/ok.bin" {
		t.Fatalf("unexpected best-effort plans: %#v", plans)
	}
	if commandErrorCode(plans[0].Err) != output.ExitNotFound {
		t.Fatalf("missing source used wrong exit code: %v", plans[0].Err)
	}
}

func TestPrepareBatchDownloadPlansDoesNotCreateDestination(t *testing.T) {
	oldRecursive := downloadRecursive
	t.Cleanup(func() { downloadRecursive = oldRecursive })
	downloadRecursive = false
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"ok": "dok"},
		lists: map[string][]driver.File{
			"dok": {{FileID: "f-ok", Name: "ok.bin"}},
		},
	}
	localDir := filepath.Join(t.TempDir(), "not-created-yet")
	plans, err := prepareBatchDownloadPlans(client, []string{"/missing.bin", "/ok/ok.bin"}, localDir)
	if err != nil || len(plans) != 2 {
		t.Fatalf("unexpected plans: %#v err=%v", plans, err)
	}
	if _, err := os.Stat(localDir); !os.IsNotExist(err) {
		t.Fatalf("planning created destination directory: %v", err)
	}
}

func TestPrepareBatchDownloadPlansReusesScopedParentResolution(t *testing.T) {
	oldRecursive := downloadRecursive
	t.Cleanup(func() { downloadRecursive = oldRecursive })
	downloadRecursive = false
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"shared": "d-shared"},
		lists: map[string][]driver.File{
			"d-shared": {
				{FileID: "f-a", Name: "a.bin"},
				{FileID: "f-b", Name: "b.bin"},
			},
		},
	}
	plans, err := prepareBatchDownloadPlans(client, []string{"/shared/a.bin", "/shared/b.bin"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Err != nil || plans[1].Err != nil {
		t.Fatalf("unexpected plans: %#v", plans)
	}
	if got := client.dirCalls["shared"]; got != 1 {
		t.Fatalf("shared parent resolved %d times, want 1", got)
	}
}

func TestBatchWorkerLimitSharesPerInterfaceBudget(t *testing.T) {
	tests := []struct {
		workers int
		jobs    int
		want    int
		wantErr bool
	}{
		{workers: 4, jobs: 2, want: 2},
		{workers: 4, jobs: 3, want: 1},
		{workers: 4, jobs: 4, want: 1},
		{workers: 4, jobs: 5, wantErr: true},
		{workers: 0, jobs: 1, wantErr: true},
		{workers: 4, jobs: 0, wantErr: true},
	}
	for _, test := range tests {
		got, err := batchWorkerLimit(test.workers, test.jobs)
		if test.wantErr {
			if err == nil {
				t.Fatalf("batchWorkerLimit(%d,%d) unexpectedly succeeded with %d", test.workers, test.jobs, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Fatalf("batchWorkerLimit(%d,%d): got=%d err=%v want=%d", test.workers, test.jobs, got, err, test.want)
		}
	}
}

func TestRunParallelBatchCapsConcurrencyAndRestoresWorkerBudget(t *testing.T) {
	oldActive := parallelBatchActiveFlag
	oldWorkers := parallelBatchWorkersPerInterface
	oldPrinter := printer
	t.Cleanup(func() {
		parallelBatchActiveFlag = oldActive
		parallelBatchWorkersPerInterface = oldWorkers
		printer = oldPrinter
	})
	parallelBatchActiveFlag = false
	parallelBatchWorkersPerInterface = 0

	var active int32
	var maxActive int32
	results := runParallelBatch(6, 2, 2, func(index int) error {
		if !batchParallelActive() {
			return errors.New("parallel batch state was not active")
		}
		if got := applyParallelBatchWorkerLimit(4); got != 2 {
			return errors.New("parallel worker budget was not applied")
		}
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maxActive)
			if current <= observed || atomic.CompareAndSwapInt32(&maxActive, observed, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	})
	for i, err := range results {
		if err != nil {
			t.Fatalf("parallel item %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maxActive); got != 2 {
		t.Fatalf("unexpected maximum concurrency: got %d want 2", got)
	}
	if batchParallelActive() {
		t.Fatal("parallel batch state leaked after completion")
	}
	if got := applyParallelBatchWorkerLimit(4); got != 4 {
		t.Fatalf("parallel worker budget leaked after completion: got %d want 4", got)
	}
}

func TestBatchUploadParallelRequiresContinueOnError(t *testing.T) {
	oldRun := uploadSingleRunE
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	t.Cleanup(func() {
		uploadSingleRunE = oldRun
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
	})
	uploadRecursive = false
	uploadContents = false
	uploadSession = ""

	root := t.TempDir()
	sources := []string{filepath.Join(root, "a.bin"), filepath.Join(root, "b.bin")}
	for _, source := range sources {
		if err := os.WriteFile(source, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	uploadSingleRunE = func(*cobra.Command, []string) error {
		t.Fatal("upload should not run when parallel failure semantics are ambiguous")
		return nil
	}
	cmd := &cobra.Command{}
	addContinueOnErrorFlag(cmd)
	addBatchJobsFlag(cmd)
	if err := cmd.Flags().Set("jobs", "2"); err != nil {
		t.Fatal(err)
	}
	err := runBatchUploadCommand(cmd, append(append([]string(nil), sources...), "/remote"))
	if err == nil || commandErrorCode(err) != output.ExitArgs || !strings.Contains(err.Error(), "requires --continue-on-error") {
		t.Fatalf("unexpected parallel fail-fast result: %v", err)
	}
}
