package cmd

import (
	"context"
	"errors"
	"net"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func classifyNetworkError(err error, fallback int) int {
	if err == nil {
		return fallback
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return output.ExitNetwork
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return output.ExitNetwork
	}
	return fallback
}

func classifyRemoteError(err error, fallback int) int {
	if err == nil {
		return fallback
	}
	if errors.Is(err, driver.ErrNotExist) || errors.Is(err, driver.ErrSharedNotFound) {
		return output.ExitNotFound
	}
	return classifyNetworkError(err, fallback)
}
