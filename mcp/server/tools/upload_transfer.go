package tools

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type mcpUploadTransferDeps struct {
	uploadFile func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error)
}

type mcpUploadTransferState struct {
	config DownloadTransferConfig
	deps   mcpUploadTransferDeps

	mu                sync.Mutex
	health            *transfer.NetworkHealthTracker
	healthCooldown    time.Duration
	healthCooldownMax time.Duration
}

func newMCPUploadTransferState() *mcpUploadTransferState {
	return &mcpUploadTransferState{
		config: DefaultDownloadTransferConfig(),
		deps:   mcpUploadTransferDeps{uploadFile: uploadpkg.UploadFile},
	}
}

func (state *mcpUploadTransferState) resetRuntimeState() {
	state.mu.Lock()
	state.health = nil
	state.healthCooldown = 0
	state.healthCooldownMax = 0
	state.mu.Unlock()
}

func (state *mcpUploadTransferState) healthTracker(config DownloadTransferConfig) (*transfer.NetworkHealthTracker, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.health != nil && state.healthCooldown == config.HealthCooldown && state.healthCooldownMax == config.HealthCooldownMax {
		return state.health, nil
	}
	health, err := transfer.NewNetworkHealthTracker(transfer.NetworkHealthOptions{
		Cooldown: config.HealthCooldown, CooldownMax: config.HealthCooldownMax,
	})
	if err != nil {
		return nil, err
	}
	state.health = health
	state.healthCooldown = config.HealthCooldown
	state.healthCooldownMax = config.HealthCooldownMax
	return health, nil
}

func (ft *FileTools) uploadThroughTransfer(ctx context.Context, dirID, fileName string, fileSize int64, file *os.File) (uploadpkg.Result, error) {
	if ft.uploadTransfer == nil {
		ft.uploadTransfer = newMCPUploadTransferState()
	}
	state := ft.uploadTransfer
	config := normalizeDownloadTransferConfig(state.config)
	if err := config.Validate(); err != nil {
		return uploadpkg.Result{}, err
	}
	if state.deps.uploadFile == nil {
		return uploadpkg.Result{}, errors.New("upload transfer implementation is nil")
	}
	if file == nil {
		return uploadpkg.Result{}, errors.New("upload file is nil")
	}
	chunkSize, err := transfer.ParseByteSize(config.ChunkSize)
	if err != nil {
		return uploadpkg.Result{}, err
	}
	if chunkSize < uploadpkg.MinPartSize {
		return uploadpkg.Result{}, errors.New("upload chunk size must be at least 100KiB")
	}
	health, err := state.healthTracker(config)
	if err != nil {
		return uploadpkg.Result{}, err
	}
	return state.deps.uploadFile(ctx, ft.client, dirID, fileName, fileSize, file, uploadpkg.Options{
		Interfaces:    config.Interfaces,
		ChunkSize:     chunkSize,
		Retries:       config.Retries,
		Timeout:       uploadpkg.DefaultTimeout,
		HealthTracker: health,
	})
}
