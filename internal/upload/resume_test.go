package upload

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func TestUploadResumeStateRoundTripAndIdentityReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload.session.json")
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	params.Callback.Callback = `{"callbackBody":"sha1=${sha1}"}`
	params.Callback.CallbackVar = `{"x:dir":"U_1_42"}`
	state.setMultipart("https://oss.example", params, "upload-id", 32<<20, false)
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}

	got, err := loadUploadResume(path, "42", "file.bin", 123, "abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.UploadID != "upload-id" || got.Params.Bucket != "bucket" || got.ChunkSize != 32<<20 {
		t.Fatalf("unexpected state: %#v", got)
	}
	if strings.Contains(string(mustReadUploadResume(t, path)), "AccessKey") || strings.Contains(string(mustReadUploadResume(t, path)), "SecurityToken") {
		t.Fatal("resume state unexpectedly contains STS credentials")
	}

	got, err = loadUploadResume(path, "42", "file.bin", 124, "ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("changed local identity reused stale state: %#v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale state was not removed: %v", err)
	}
}

func TestUploadResumeStateRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := loadUploadResume(link, "1", "a", 1, "A")
	if !errors.Is(err, ErrUploadResumeState) {
		t.Fatalf("expected resume symlink rejection, got %v", err)
	}
}

func TestValidateSequentialResumePartsRequiresContiguousPrefix(t *testing.T) {
	if err := validateSequentialResumeParts([]oss.UploadPart{{PartNumber: 1}, {PartNumber: 2}}); err != nil {
		t.Fatalf("contiguous prefix should resume: %v", err)
	}
	if err := validateSequentialResumeParts([]oss.UploadPart{{PartNumber: 1}, {PartNumber: 3}}); !errors.Is(err, ErrUploadResumeState) {
		t.Fatalf("expected gap rejection, got %v", err)
	}
}

func TestRunUploadWithRecoveryRetriesVerificationFailure(t *testing.T) {
	var attempts int
	var waits []int
	var statuses []string
	var progress []int64
	options := DefaultOptions()
	options.Retries = 3
	options.Progress = func(message string) { statuses = append(statuses, message) }
	options.ProgressBytes = func(completed, _ int64) { progress = append(progress, completed) }

	result, err := runUploadWithRecovery(context.Background(), options.Retries, options, 100, func(attempt int) (Result, error) {
		attempts++
		if attempt == 0 {
			return Result{SHA1: "ABC"}, driver.ErrUploadVerificationFailed
		}
		return Result{SHA1: "ABC", BytesUploaded: 100}, nil
	}, func(_ context.Context, retryNumber int) error {
		waits = append(waits, retryNumber)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(waits) != 1 || waits[0] != 1 {
		t.Fatalf("unexpected recovery loop: attempts=%d waits=%v", attempts, waits)
	}
	if result.BytesUploaded != 100 || len(statuses) != 1 || !strings.Contains(statuses[0], "retry 1/3") {
		t.Fatalf("unexpected recovery result/status: result=%#v statuses=%v", result, statuses)
	}
	if len(progress) != 1 || progress[0] != 0 {
		t.Fatalf("recovery should reset absolute byte progress before retry: %v", progress)
	}
}

func TestUploadCompatibilityStateLatchesSequentialAcrossOptionCopies(t *testing.T) {
	options := DefaultOptions()
	copyBefore := options
	if copyBefore.Compatibility.SequentialRequired() {
		t.Fatal("compatibility state unexpectedly starts in sequential mode")
	}
	options.Compatibility.RequireSequential()
	copyAfter := options
	if !copyBefore.Compatibility.SequentialRequired() || !copyAfter.Compatibility.SequentialRequired() {
		t.Fatal("compatibility latch was not shared across option copies")
	}
}

func TestUploadCompatibilityStateRemembersOnePathPerPhysicalInterface(t *testing.T) {
	state := NewUploadCompatibilityState()
	state.ObserveNetworkPaths([]transfer.NetworkPath{
		{InterfaceName: "NIC-2", InterfaceIndex: 2, LocalIP: net.ParseIP("10.0.2.1")},
		{InterfaceName: "NIC-1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.1.1")},
		{InterfaceName: "NIC-1", InterfaceIndex: 1, LocalIP: net.ParseIP("10.0.1.2")},
	})
	paths := state.NetworkPaths()
	if len(paths) != 2 || paths[0].InterfaceIndex != 1 || paths[1].InterfaceIndex != 2 {
		t.Fatalf("unexpected remembered physical interfaces: %#v", paths)
	}
	if !paths[0].LocalIP.Equal(net.ParseIP("10.0.1.1")) {
		t.Fatalf("first observed path for interface was not kept stable: %#v", paths[0])
	}
}

func TestRunUploadWithRecoveryDoesNotRetryPermanentFailure(t *testing.T) {
	attempts := 0
	options := DefaultOptions()
	_, err := runUploadWithRecovery(context.Background(), 3, options, 100, func(int) (Result, error) {
		attempts++
		return Result{}, driver.ErrUploadTooLarge
	}, func(context.Context, int) error {
		t.Fatal("permanent error unexpectedly entered recovery backoff")
		return nil
	})
	if !errors.Is(err, driver.ErrUploadTooLarge) || attempts != 1 {
		t.Fatalf("permanent failure was retried: attempts=%d err=%v", attempts, err)
	}
}

func TestUploadVerificationFailureClassifierHandlesNil(t *testing.T) {
	if isUploadVerificationFailure(nil) || isRecoverableUploadFailure(nil) {
		t.Fatal("nil upload error must not be classified as verification or recoverable failure")
	}
}

func TestUploadVerificationFailureClassifierKeepsRaw10002Compatibility(t *testing.T) {
	err := errors.New(`{"state":false,"message":"校验文件失败，请重新上传。","code":10002}`)
	if !isUploadVerificationFailure(err) || !isRecoverableUploadFailure(err) {
		t.Fatalf("raw 10002 callback should be recoverable: %v", err)
	}
}

func TestMigrateParallelSHA1ResumeToSequentialBeforeOSSReuse(t *testing.T) {
	options := DefaultOptions()
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	params.Callback.Callback = `{"callbackBody":"sha1=${sha1}"}`
	state.setMultipart("https://oss.example", params, "unsafe-parallel-upload", 32<<20, false)

	if !migrateParallelSHA1ResumeToSequential(&options, &state) {
		t.Fatal("legacy parallel ${sha1} session was not migrated before OSS reuse")
	}
	if state.Phase != uploadResumePhasePrepared || state.UploadID != "" || !state.ForceSequential {
		t.Fatalf("callback migration retained unsafe multipart state: %#v", state)
	}
	if !options.forceSequential || !options.Compatibility.SequentialRequired() {
		t.Fatalf("callback migration did not latch sequential compatibility: %#v", options)
	}
}

func TestPrepareMissingParallelMultipartRetryMigratesLegacyStateToSequential(t *testing.T) {
	options := DefaultOptions()
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	state.setMultipart("https://oss.example", params, "old-parallel-upload", 32<<20, false)

	prepareMissingMultipartRetry(&options, &state)
	if state.Phase != uploadResumePhasePrepared || state.UploadID != "" || !state.ForceSequential {
		t.Fatalf("legacy missing multipart was not migrated: %#v", state)
	}
	if !options.forceSequential || !options.Compatibility.SequentialRequired() {
		t.Fatalf("legacy migration did not latch current-process compatibility mode: %#v", options)
	}
}

func TestResetUploadResumeAfterVerificationFailureForcesFreshInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload.session.json")
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	state.setMultipart("https://oss.example", params, "stale-upload-id", 32<<20, false)
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}
	if err := resetUploadResumeAfterVerificationFailure(path, &state); err != nil {
		t.Fatal(err)
	}
	got, err := loadUploadResume(path, "42", "file.bin", 123, "ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Phase != uploadResumePhasePrepared || got.UploadID != "" || got.Params.Bucket != "" || got.ChunkSize != 0 || !got.ForceSequential {
		t.Fatalf("verification recovery retained stale multipart context or lost compatibility latch: %#v", got)
	}
}

func mustReadUploadResume(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
