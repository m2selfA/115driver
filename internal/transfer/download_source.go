package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

const DefaultDownloadURLRefreshes = 3

var (
	ErrDownloadSourceExpired = errors.New("download source expired or unauthorized")
	ErrDownloadSourceRefresh = errors.New("download source refresh failed")
)

// DownloadSource is a signed CDN URL together with the exact request headers
// required to use it. Headers are cloned when the source enters the transfer
// layer so callers can safely reuse or replace their own maps.
type DownloadSource struct {
	URL    string
	Header http.Header
}

// DownloadSourceRefreshFunc obtains a fresh signed CDN source for the same
// logical file. Implementations normally call the authenticated 115 download
// API. The transfer package deliberately does not depend on pkg/driver.
type DownloadSourceRefreshFunc func(context.Context) (DownloadSource, error)

type downloadSourceSnapshot struct {
	URL     string
	Header  http.Header
	Version uint64
}

type downloadSourceState struct {
	mu           sync.Mutex
	source       DownloadSource
	refresh      DownloadSourceRefreshFunc
	maxRefreshes int
	refreshes    int
	version      uint64
}

func newDownloadSourceState(initial DownloadSource, refresh DownloadSourceRefreshFunc, maxRefreshes int) (*downloadSourceState, error) {
	if maxRefreshes < 0 {
		return nil, errors.New("download URL refresh count must be >= 0")
	}
	if _, err := parseCDNProbeURL(initial.URL); err != nil {
		return nil, err
	}
	return &downloadSourceState{
		source:  DownloadSource{URL: initial.URL, Header: cloneHTTPHeader(initial.Header)},
		refresh: refresh, maxRefreshes: maxRefreshes,
	}, nil
}

func (state *downloadSourceState) current() downloadSourceSnapshot {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.snapshotLocked()
}

// refreshIfStale serializes refreshes across concurrent chunk workers. If
// another worker has already advanced the source version, the caller simply
// receives that newer source without consuming another refresh allowance.
func (state *downloadSourceState) refreshIfStale(ctx context.Context, staleVersion uint64) (downloadSourceSnapshot, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.version != staleVersion {
		return state.snapshotLocked(), nil
	}
	if state.refresh == nil {
		return downloadSourceSnapshot{}, ErrDownloadSourceExpired
	}
	if state.refreshes >= state.maxRefreshes {
		return downloadSourceSnapshot{}, fmt.Errorf("%w: refresh limit %d reached", ErrDownloadSourceRefresh, state.maxRefreshes)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return downloadSourceSnapshot{}, err
	}

	state.refreshes++
	fresh, err := state.refresh(ctx)
	if err != nil {
		return downloadSourceSnapshot{}, fmt.Errorf("%w: %w", ErrDownloadSourceRefresh, err)
	}
	if _, err := parseCDNProbeURL(fresh.URL); err != nil {
		return downloadSourceSnapshot{}, fmt.Errorf("%w: invalid refreshed source: %v", ErrDownloadSourceRefresh, err)
	}
	state.source = DownloadSource{URL: fresh.URL, Header: cloneHTTPHeader(fresh.Header)}
	state.version++
	return state.snapshotLocked(), nil
}

func (state *downloadSourceState) refreshCount() int {
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.refreshes
}

func (state *downloadSourceState) snapshotLocked() downloadSourceSnapshot {
	return downloadSourceSnapshot{
		URL: state.source.URL, Header: cloneHTTPHeader(state.source.Header), Version: state.version,
	}
}

func cloneHTTPHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

func isDownloadSourceExpiredStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusGone
}

func sourceExpiredError(status int) error {
	return errors.Join(
		fmt.Errorf("%w: HTTP %d", ErrDownloadSourceExpired, status),
		fmt.Errorf("%w: %d", ErrUnexpectedDownloadStatus, status),
	)
}

func isTerminalDownloadSourceError(err error) bool {
	return errors.Is(err, ErrDownloadSourceExpired) || errors.Is(err, ErrDownloadSourceRefresh)
}
