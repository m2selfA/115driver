package upload

import (
	"errors"
	"fmt"
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

type Options struct {
	Interfaces    string
	ChunkSize     int64
	Retries       int
	Timeout       time.Duration
	HealthTracker *transfer.NetworkHealthTracker
	Progress      func(string)
}

func DefaultOptions() Options {
	return Options{
		Interfaces: DefaultInterfaces,
		ChunkSize:  DefaultChunkSize,
		Retries:    transfer.DefaultUploadPartRetries,
		Timeout:    DefaultTimeout,
	}
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
	if options.Timeout < 0 {
		return errors.New("upload timeout must be >= 0")
	}
	return nil
}
