package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// InspectRemoteResumeMultipart reports whether one persistent resume state
// describes an active OSS multipart upload. It only reads and validates local
// resume metadata; it performs no network requests and does not mutate state.
func InspectRemoteResumeMultipart(resumePath string) (bool, error) {
	_, active, err := loadMultipartAbortState(resumePath)
	return active, err
}

// AbortRemoteResumeMultipart aborts the OSS multipart upload described by one
// persistent resume state. It never removes or mutates the local resume file;
// callers should only discard local state after all requested remote aborts
// have succeeded. The returned bool reports whether the state described an
// active multipart upload that required abort handling.
func AbortRemoteResumeMultipart(ctx context.Context, client *driver.Pan115Client, resumePath string, options Options) (bool, error) {
	state, active, err := loadMultipartAbortState(resumePath)
	if err != nil || !active {
		return active, err
	}
	if client == nil {
		return true, fmt.Errorf("abort remote multipart: client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	params := state.uploadParams()
	selection, err := resolveUploadPathsForParams(ctx, options, state.Endpoint, params)
	if err != nil {
		return true, fmt.Errorf("resolve network paths for remote multipart abort: %w", err)
	}
	pool := newOSSBucketPool(client, state.Endpoint, params.Bucket)
	defer pool.close()
	imur := oss.InitiateMultipartUploadResult{Bucket: params.Bucket, Key: params.Object, UploadID: state.UploadID}
	if err := abortMultipart(pool, selection.Paths, imur); err != nil {
		return true, fmt.Errorf("abort remote multipart %s: %w", state.UploadID, err)
	}
	return true, nil
}

func loadMultipartAbortState(path string) (uploadResumeState, bool, error) {
	if strings.TrimSpace(path) == "" {
		return uploadResumeState{}, false, nil
	}
	if err := rejectUploadResumeSymlink(path); err != nil {
		return uploadResumeState{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return uploadResumeState{}, false, nil
	}
	if err != nil {
		return uploadResumeState{}, false, fmt.Errorf("read upload resume state for remote abort: %w", err)
	}
	var state uploadResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return uploadResumeState{}, false, fmt.Errorf("%w: decode %s for remote abort: %v", ErrUploadResumeState, path, err)
	}
	if state.Version != uploadResumeVersion {
		return uploadResumeState{}, false, fmt.Errorf("%w: unsupported version %d", ErrUploadResumeState, state.Version)
	}
	if state.Phase != uploadResumePhaseMultipart {
		return state, false, nil
	}
	if strings.TrimSpace(state.Endpoint) == "" || strings.TrimSpace(state.Params.Bucket) == "" || strings.TrimSpace(state.Params.Object) == "" || strings.TrimSpace(state.UploadID) == "" {
		return uploadResumeState{}, true, fmt.Errorf("%w: multipart state is incomplete", ErrUploadResumeState)
	}
	return state, true, nil
}
