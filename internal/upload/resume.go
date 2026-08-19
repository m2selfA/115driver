package upload

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

const uploadResumeVersion = 1

var ErrUploadResumeState = errors.New("upload resume state is invalid")

const (
	uploadResumePhasePrepared  = "prepared"
	uploadResumePhaseMultipart = "multipart"
	uploadResumePhaseCompleted = "completed"
)

type uploadResumeParams struct {
	Bucket      string `json:"bucket"`
	Object      string `json:"object"`
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

type uploadResumeState struct {
	Version    int                `json:"version"`
	DirID      string             `json:"dir_id"`
	FileName   string             `json:"file_name"`
	FileSize   int64              `json:"file_size"`
	SHA1       string             `json:"sha1"`
	Phase      string             `json:"phase"`
	Endpoint   string             `json:"endpoint,omitempty"`
	Params     uploadResumeParams `json:"params,omitempty"`
	UploadID   string             `json:"upload_id,omitempty"`
	ChunkSize  int64              `json:"chunk_size,omitempty"`
	Sequential bool               `json:"sequential,omitempty"`
}

func loadUploadResume(path, dirID, fileName string, fileSize int64, sha1 string) (*uploadResumeState, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if err := rejectUploadResumeSymlink(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read upload resume state: %w", err)
	}
	var state uploadResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrUploadResumeState, filepath.Base(path), err)
	}
	if state.Version != uploadResumeVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrUploadResumeState, state.Version)
	}
	if !state.matches(dirID, fileName, fileSize, sha1) {
		if err := removeUploadResume(path); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if state.Phase != uploadResumePhasePrepared && state.Phase != uploadResumePhaseMultipart && state.Phase != uploadResumePhaseCompleted {
		return nil, fmt.Errorf("%w: unsupported phase %q", ErrUploadResumeState, state.Phase)
	}
	return &state, nil
}

func saveUploadResume(path string, state uploadResumeState) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create upload resume directory: %w", err)
	}
	if err := rejectUploadResumeSymlink(path); err != nil {
		return err
	}
	state.Version = uploadResumeVersion
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upload resume state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create upload resume temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure upload resume temp file: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write upload resume state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync upload resume state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close upload resume state: %w", err)
	}
	if err := replaceUploadStateFile(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func removeUploadResume(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := rejectUploadResumeSymlink(path); err != nil {
		return err
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RemoveResumeState removes a persistent upload resume state after the caller
// has durably observed success. Failed/interrupted uploads should keep the state.
func RemoveResumeState(path string) error {
	return removeUploadResume(path)
}

func (state uploadResumeState) matches(dirID, fileName string, fileSize int64, sha1 string) bool {
	return state.DirID == dirID && state.FileName == fileName && state.FileSize == fileSize && strings.EqualFold(state.SHA1, sha1)
}

func newPreparedUploadResume(dirID, fileName string, fileSize int64, sha1 string) uploadResumeState {
	return uploadResumeState{
		Version: uploadResumeVersion, DirID: dirID, FileName: fileName, FileSize: fileSize, SHA1: sha1, Phase: uploadResumePhasePrepared,
	}
}

func (state *uploadResumeState) setMultipart(endpoint string, params driver.UploadOSSParams, uploadID string, chunkSize int64, sequential bool) {
	state.Phase = uploadResumePhaseMultipart
	state.Endpoint = endpoint
	state.Params = uploadResumeParams{
		Bucket: params.Bucket, Object: params.Object, Callback: params.Callback.Callback, CallbackVar: params.Callback.CallbackVar,
	}
	state.UploadID = uploadID
	state.ChunkSize = chunkSize
	state.Sequential = sequential
}

func (state uploadResumeState) uploadParams() driver.UploadOSSParams {
	params := driver.UploadOSSParams{SHA1: state.SHA1, Bucket: state.Params.Bucket, Object: state.Params.Object}
	params.Callback.Callback = state.Params.Callback
	params.Callback.CallbackVar = state.Params.CallbackVar
	return params
}

func rejectUploadResumeSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: state file %q is a symbolic link", ErrUploadResumeState, filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: state path %q is not a regular file", ErrUploadResumeState, filepath.Base(path))
	}
	return nil
}
