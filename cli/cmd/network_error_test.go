package cmd

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestClassifyNetworkErrorOnlyOverridesNetworkFailures(t *testing.T) {
	if got := classifyNetworkError(&url.Error{Op: "Get", URL: "https://example.invalid", Err: errors.New("down")}, output.ExitAuth); got != output.ExitNetwork {
		t.Fatalf("url.Error classified as %d, want ExitNetwork", got)
	}
	if got := classifyNetworkError(context.DeadlineExceeded, output.ExitError); got != output.ExitNetwork {
		t.Fatalf("deadline classified as %d, want ExitNetwork", got)
	}
	if got := classifyNetworkError(driver.ErrBadCookie, output.ExitAuth); got != output.ExitAuth {
		t.Fatalf("bad cookie classified as %d, want ExitAuth", got)
	}
	if got := classifyNetworkError(driver.ErrUnexpected, output.ExitError); got != output.ExitError {
		t.Fatalf("protocol error classified as %d, want fallback", got)
	}
}

func TestClassifyRemoteErrorDistinguishesNotFoundNetworkAndProtocolFailures(t *testing.T) {
	if got := classifyRemoteError(driver.ErrNotExist, output.ExitError); got != output.ExitNotFound {
		t.Fatalf("remote not-found classified as %d, want ExitNotFound", got)
	}
	if got := classifyRemoteError(driver.ErrSharedNotFound, output.ExitError); got != output.ExitNotFound {
		t.Fatalf("shared not-found classified as %d, want ExitNotFound", got)
	}
	if got := classifyRemoteError(&url.Error{Op: "Get", URL: "https://example.invalid", Err: errors.New("down")}, output.ExitError); got != output.ExitNetwork {
		t.Fatalf("remote network failure classified as %d, want ExitNetwork", got)
	}
	if got := classifyRemoteError(driver.ErrUnexpected, output.ExitError); got != output.ExitError {
		t.Fatalf("remote protocol failure classified as %d, want ExitError", got)
	}
}
