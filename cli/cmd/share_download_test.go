package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type fakeShareDownloadTransferClient struct {
	mu        sync.Mutex
	responses map[string][]*driver.SharedDownloadRequest
	errors    map[string]error
	calls     []string
}

func (f *fakeShareDownloadTransferClient) DownloadByShareCodeRequest(shareCode, receiveCode, fileID string) (*driver.SharedDownloadRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, shareCode+":"+receiveCode+":"+fileID)
	if err := f.errors[fileID]; err != nil {
		return nil, err
	}
	sequence := f.responses[fileID]
	if len(sequence) == 0 {
		return nil, errors.New("no fake share response")
	}
	info := sequence[0]
	if len(sequence) > 1 {
		f.responses[fileID] = sequence[1:]
	}
	return info, nil
}

func testSharedDownloadInfo(id, name string, size int64, rawURL string, headers http.Header) *driver.SharedDownloadRequest {
	info := &driver.SharedDownloadRequest{
		SharedDownloadInfo: driver.SharedDownloadInfo{FileID: id, FileName: name, FileSize: driver.StringInt64(size)},
		Header:             headers.Clone(),
	}
	info.URL.URL = rawURL
	return info
}

func newShareDownloadTestCommand(t *testing.T, fromFile string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "download"}
	addContinueOnErrorFlag(cmd)
	addBatchJobsFlag(cmd)
	addBatchFromFileFlag(cmd)
	cmd.Flags().Int("workers-per-interface", 0, "test override")
	if fromFile != "" {
		if err := cmd.Flags().Set("from-file", fromFile); err != nil {
			t.Fatal(err)
		}
	}
	return cmd
}

func preserveShareDownloadGlobals(t *testing.T) {
	t.Helper()
	oldReceive := shareDownloadReceiveCode
	oldTimeout := shareDownloadTimeout
	oldInterfaces := shareDownloadInterfaces
	oldStrategy := shareDownloadStrategy
	oldChunk := shareDownloadChunkSize
	oldWorkers := shareDownloadWorkersPerInterface
	oldDryRun := shareDownloadDryRun
	oldConfig := configPath
	oldJSON := jsonOutput
	oldPrinter := printer
	t.Cleanup(func() {
		shareDownloadReceiveCode = oldReceive
		shareDownloadTimeout = oldTimeout
		shareDownloadInterfaces = oldInterfaces
		shareDownloadStrategy = oldStrategy
		shareDownloadChunkSize = oldChunk
		shareDownloadWorkersPerInterface = oldWorkers
		shareDownloadDryRun = oldDryRun
		configPath = oldConfig
		jsonOutput = oldJSON
		printer = oldPrinter
	})
}

func TestShareDownloadArgsRejectsPureInputErrorsBeforeAuthentication(t *testing.T) {
	t.Setenv(envShareReceiveCode, "")
	preserveShareDownloadGlobals(t)
	shareDownloadReceiveCode = "code"
	shareDownloadTimeout = defaultDownloadTimeout
	shareDownloadStrategy = ""
	shareDownloadChunkSize = ""
	shareDownloadWorkersPerInterface = 0

	for name, configure := range map[string]func(*cobra.Command){
		"missing-receive": func(cmd *cobra.Command) { shareDownloadReceiveCode = "" },
		"bad-strategy":    func(cmd *cobra.Command) { shareDownloadStrategy = "sideways" },
		"bad-chunk":       func(cmd *cobra.Command) { shareDownloadChunkSize = "not-a-size" },
		"single-jobs": func(cmd *cobra.Command) {
			if err := cmd.Flags().Set("jobs", "2"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			shareDownloadReceiveCode = "code"
			shareDownloadStrategy = ""
			shareDownloadChunkSize = ""
			cmd := newShareDownloadTestCommand(t, "")
			configure(cmd)
			err := shareDownloadArgs(cmd, []string{"share", "file-1", "target.bin"})
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("share download args error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestShareDownloadArgsAcceptsReceiveCodeFromEnvironment(t *testing.T) {
	preserveShareDownloadGlobals(t)
	t.Setenv(envShareReceiveCode, "env-secret")
	shareDownloadReceiveCode = ""
	shareDownloadTimeout = defaultDownloadTimeout
	shareDownloadStrategy = ""
	shareDownloadChunkSize = ""
	shareDownloadWorkersPerInterface = 0
	cmd := newShareDownloadTestCommand(t, "")
	if err := shareDownloadArgs(cmd, []string{"share", "file-1", "target.bin"}); err != nil {
		t.Fatalf("environment receive code rejected: %v", err)
	}
}

func TestPrepareShareDownloadPlansRejectsDestinationCollision(t *testing.T) {
	client := &fakeShareDownloadTransferClient{responses: map[string][]*driver.SharedDownloadRequest{
		"f1": {testSharedDownloadInfo("f1", "same.bin", 10, "https://cdn.invalid/1", nil)},
		"f2": {testSharedDownloadInfo("f2", "same.bin", 20, "https://cdn.invalid/2", nil)},
	}}
	_, err := prepareShareDownloadPlans(context.Background(), client, "share", "code", []string{"f1", "f2"}, filepath.Join(t.TempDir(), "downloads"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "same local path") {
		t.Fatalf("collision error = %T %v, want ExitArgs collision", err, err)
	}
}

func TestShareDownloadDryRunDoesNotCreateLocalPaths(t *testing.T) {
	preserveShareDownloadGlobals(t)
	shareDownloadReceiveCode = "secret-code"
	shareDownloadTimeout = defaultDownloadTimeout
	shareDownloadDryRun = true
	shareDownloadStrategy = "file"
	shareDownloadChunkSize = ""
	shareDownloadInterfaces = ""
	shareDownloadWorkersPerInterface = 0
	configPath = filepath.Join(t.TempDir(), "missing.toml")
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeShareDownloadTransferClient{responses: map[string][]*driver.SharedDownloadRequest{
		"f1": {testSharedDownloadInfo("f1", "a.bin", 10, "https://cdn.invalid/a", http.Header{"Referer": {"secret-referer"}})},
		"f2": {testSharedDownloadInfo("f2", "b.bin", 20, "https://cdn.invalid/b", http.Header{"Cookie": {"secret-cookie"}})},
	}}
	target := filepath.Join(t.TempDir(), "does-not-exist")
	cmd := newShareDownloadTestCommand(t, "")
	if err := runShareDownloadCommandWithClient(client, cmd, []string{"share-code", "f1", "f2", target}, downloadPipelineDeps{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run created local target %q: %v", target, err)
	}
	for _, call := range client.calls {
		if !strings.Contains(call, ":secret-code:") {
			t.Fatalf("share receive code was not passed to driver: %q", call)
		}
	}
}

func TestShareDownloadSequentialFailureLeavesLaterItemsUnprocessed(t *testing.T) {
	preserveShareDownloadGlobals(t)
	shareDownloadReceiveCode = "secret-code"
	shareDownloadTimeout = defaultDownloadTimeout
	shareDownloadDryRun = false
	shareDownloadStrategy = "file"
	shareDownloadChunkSize = ""
	shareDownloadInterfaces = "auto"
	shareDownloadWorkersPerInterface = 0
	configPath = filepath.Join(t.TempDir(), "missing.toml")
	jsonOutput = true
	printer = output.NewPrinter(false)

	client := &fakeShareDownloadTransferClient{responses: map[string][]*driver.SharedDownloadRequest{
		"f1": {testSharedDownloadInfo("f1", "a.bin", 10, "https://cdn.invalid/a", nil)},
		"f2": {testSharedDownloadInfo("f2", "b.bin", 20, "https://cdn.invalid/b", nil)},
	}}
	path := testCLIPath("Ethernet", 1, "10.0.0.1")
	deps := downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{path}}, nil
		},
		probeCDNPaths: func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.CDNDiscoveryResult{Host: "cdn.invalid", Paths: []transfer.NetworkPath{path}}, nil
		},
		scheduleFiles: func(context.Context, []transfer.NetworkPath, []transfer.FileTransferJob, ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			return transfer.FileScheduleReport{}, errors.New("first transfer failed")
		},
	}
	cmd := newShareDownloadTestCommand(t, "")
	err := runShareDownloadCommandWithClient(client, cmd, []string{"share-code", "f1", "f2", filepath.Join(t.TempDir(), "downloads")}, deps)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitError {
		t.Fatalf("share batch error = %T %v, want ExitError", err, err)
	}
	data, ok := ee.data.(map[string]interface{})
	if !ok {
		t.Fatalf("share batch error data = %T, want map", ee.data)
	}
	if data["processed"] != 1 || data["failed"] != 1 || data["succeeded"] != 0 || data["remaining"] != 1 {
		t.Fatalf("unexpected partial accounting: %#v", data)
	}
	items, ok := data["items"].([]batchItemResult)
	if !ok || len(items) != 1 || items[0].Input != "f1" || items[0].Success {
		t.Fatalf("unexpected processed items: %#v", data["items"])
	}
}

func TestFinishShareDownloadBatchCarriesDryRunStrategy(t *testing.T) {
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() { jsonOutput, printer = oldJSON, oldPrinter })
	jsonOutput = true
	printer = output.NewPrinter(false)
	plans := []shareDownloadPlan{
		{FileID: "f1", Info: testSharedDownloadInfo("f1", "a.bin", 10, "https://cdn.invalid/a", nil), Destination: "a.bin"},
		{FileID: "missing", Err: &exitError{code: output.ExitNotFound, msg: "missing"}},
	}
	err := finishShareDownloadBatch("share-code", plans, nil, len(plans), 1, 0, true, "chunk")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("dry-run partial result = %T %v, want exitError", err, err)
	}
	data := ee.data.(map[string]interface{})
	items := data["items"].([]batchItemResult)
	first := items[0].Data.(shareDownloadItemResult)
	if !first.DryRun || first.Strategy != "chunk" {
		t.Fatalf("dry-run item lost strategy metadata: %#v", first)
	}
}

func TestExecuteShareDownloadPlanUsesHeadersAndRefreshesSameFile(t *testing.T) {
	initialHeader := http.Header{"Referer": {"https://115.com/s/share?password=secret"}, "Cookie": {"token=abc"}}
	freshHeader := http.Header{"Referer": {"https://115.com/s/share?password=secret"}, "Cookie": {"token=def"}}
	client := &fakeShareDownloadTransferClient{responses: map[string][]*driver.SharedDownloadRequest{
		"f1": {testSharedDownloadInfo("f1", "a.bin", 12, "https://cdn.invalid/fresh", freshHeader)},
	}}
	plan := shareDownloadPlan{FileID: "f1", Info: testSharedDownloadInfo("f1", "a.bin", 12, "https://cdn.invalid/initial", initialHeader), Destination: filepath.Join(t.TempDir(), "a.bin")}
	path := testCLIPath("Ethernet", 1, "10.0.0.1")
	selection := downloadPathSelection{Workers: []transfer.NetworkPath{path}, Candidates: []transfer.NetworkPath{path}}
	options := testDownloadCommandOptions(false)
	options.Resume = false
	options.Strategy = "file"
	options.URLRefreshes = 1
	var probedHeaders []http.Header
	deps := downloadPipelineDeps{
		probeCDNPaths: func(_ context.Context, rawURL string, headers http.Header, candidates []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			probedHeaders = append(probedHeaders, headers.Clone())
			return transfer.CDNDiscoveryResult{Host: "cdn.invalid", Paths: []transfer.NetworkPath{path}}, nil
		},
		scheduleFiles: func(ctx context.Context, paths []transfer.NetworkPath, jobs []transfer.FileTransferJob, opts ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if len(jobs) != 1 || jobs[0].Header.Get("Cookie") != "token=abc" || jobs[0].Header.Get("Referer") != initialHeader.Get("Referer") {
				t.Fatalf("share download job lost request context: %#v", jobs)
			}
			fresh, err := jobs[0].Refresh(ctx)
			if err != nil {
				t.Fatalf("refresh failed: %v", err)
			}
			if fresh.URL != "https://cdn.invalid/fresh" || fresh.Header.Get("Cookie") != "token=def" {
				t.Fatalf("unexpected refreshed source: %#v", fresh)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{
				JobID: jobs[0].ID, DestinationPath: jobs[0].DestinationPath, ExpectedSize: jobs[0].ExpectedSize,
				Result: transfer.FileDownloadResult{DestinationPath: jobs[0].DestinationPath, BytesWritten: 12},
			}}}, nil
		},
	}
	result, err := executeShareDownloadPlan(context.Background(), client, "share-code", "secret-code", plan, options, selection, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transferred != 12 || result.FileID != "f1" || result.LocalPath != plan.Destination {
		t.Fatalf("unexpected share download result: %#v", result)
	}
	if len(probedHeaders) != 2 || probedHeaders[0].Get("Cookie") != "token=abc" || probedHeaders[1].Get("Cookie") != "token=def" {
		t.Fatalf("share CDN probes did not preserve headers across refresh: %#v", probedHeaders)
	}
	if len(client.calls) != 1 || !strings.HasSuffix(client.calls[0], ":f1") {
		t.Fatalf("refresh did not stay bound to f1: %#v", client.calls)
	}
}
