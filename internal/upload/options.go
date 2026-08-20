package upload

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const (
	DefaultInterfaces = "auto"
	DefaultChunkSize  = int64(32 << 20)
	DefaultTimeout    = 24 * time.Hour
	MinPartSize       = int64(100 << 10)
	MaxPartCount      = 10000
)

type UploadCompatibilityState struct {
	sequential atomic.Bool
	mu         sync.Mutex
	paths      map[int]transfer.NetworkPath
}

func NewUploadCompatibilityState() *UploadCompatibilityState {
	return &UploadCompatibilityState{paths: make(map[int]transfer.NetworkPath)}
}

func (state *UploadCompatibilityState) RequireSequential() {
	if state != nil {
		state.sequential.Store(true)
	}
}

func (state *UploadCompatibilityState) SequentialRequired() bool {
	return state != nil && state.sequential.Load()
}

// ObserveNetworkPaths remembers one reachable source path per physical
// interface. Recursive uploads use this after a sequential-protocol decision
// to distribute independent files across NICs without parallelizing the parts
// of a single sequential object.
func (state *UploadCompatibilityState) ObserveNetworkPaths(paths []transfer.NetworkPath) {
	if state == nil || len(paths) == 0 {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.paths == nil {
		state.paths = make(map[int]transfer.NetworkPath)
	}
	for _, path := range paths {
		if path.InterfaceIndex <= 0 {
			continue
		}
		if _, exists := state.paths[path.InterfaceIndex]; !exists {
			state.paths[path.InterfaceIndex] = path
		}
	}
}

func (state *UploadCompatibilityState) NetworkPaths() []transfer.NetworkPath {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	paths := make([]transfer.NetworkPath, 0, len(state.paths))
	for _, path := range state.paths {
		paths = append(paths, path)
	}
	state.mu.Unlock()
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].InterfaceIndex != paths[j].InterfaceIndex {
			return paths[i].InterfaceIndex < paths[j].InterfaceIndex
		}
		return paths[i].LocalIP.String() < paths[j].LocalIP.String()
	})
	return paths
}

type Options struct {
	Interfaces          string
	ChunkSize           int64
	Retries             int
	WorkersPerInterface int
	Timeout             time.Duration
	HealthTracker       *transfer.NetworkHealthTracker
	Compatibility       *UploadCompatibilityState
	Progress            func(string)
	ProgressBytes       func(completed, total int64)
	ResumePath          string
	PreparedDigest      *PreparedDigest
	forceSequential     bool
}

func DefaultOptions() Options {
	return Options{
		Interfaces:          DefaultInterfaces,
		ChunkSize:           DefaultChunkSize,
		Retries:             transfer.DefaultUploadPartRetries,
		WorkersPerInterface: 1,
		Timeout:             DefaultTimeout,
		Compatibility:       NewUploadCompatibilityState(),
	}
}

func (options Options) reportBytes(completed, total int64) {
	if options.ProgressBytes == nil {
		return
	}
	if total < 0 {
		total = 0
	}
	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}
	options.ProgressBytes(completed, total)
}

func (options Options) validate() error {
	if options.Interfaces == "" {
		return errors.New("upload interfaces must not be empty")
	}
	if options.ChunkSize < MinPartSize {
		return fmt.Errorf("upload chunk size must be at least %d bytes", MinPartSize)
	}
	if options.Retries < 0 {
		return errors.New("upload retries must be >= 0")
	}
	if options.WorkersPerInterface <= 0 {
		return errors.New("upload workers per interface must be > 0")
	}
	if options.Timeout < 0 {
		return errors.New("upload timeout must be >= 0")
	}
	return nil
}
