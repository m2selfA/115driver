package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	DefaultNetworkHealthCooldown    = 5 * time.Second
	DefaultNetworkHealthCooldownMax = time.Minute
	defaultNetworkHealthScore       = 100
	networkHealthFailurePenalty     = 25
	networkHealthSuccessReward      = 10
)

// ErrNetworkPathFailure marks a transfer error that represents transient path
// health rather than an object, protocol, or local-filesystem failure.
var ErrNetworkPathFailure = errors.New("network path failure")

// NetworkHealthOptions configures exponential interface cooldown.
type NetworkHealthOptions struct {
	Cooldown    time.Duration
	CooldownMax time.Duration
}

// DefaultNetworkHealthOptions returns P8's default cooldown policy.
func DefaultNetworkHealthOptions() NetworkHealthOptions {
	return NetworkHealthOptions{Cooldown: DefaultNetworkHealthCooldown, CooldownMax: DefaultNetworkHealthCooldownMax}
}

func (options NetworkHealthOptions) validate() error {
	if options.Cooldown <= 0 {
		return errors.New("network health cooldown must be > 0")
	}
	if options.CooldownMax < options.Cooldown {
		return errors.New("network health maximum cooldown must be >= cooldown")
	}
	return nil
}

// NetworkHealthSnapshot is a safe diagnostic view of one physical interface.
type NetworkHealthSnapshot struct {
	InterfaceName       string
	InterfaceIndex      int
	Score               int
	ConsecutiveFailures int
	Successes           uint64
	Failures            uint64
	LastSuccess         time.Time
	LastFailure         time.Time
	CooldownUntil       time.Time
	InCooldown          bool
}

type networkHealthState struct {
	interfaceName       string
	score               int
	consecutiveFailures int
	successes           uint64
	failures            uint64
	lastSuccess         time.Time
	lastFailure         time.Time
	cooldownUntil       time.Time
}

// NetworkHealthTracker shares health across every address on the same physical
// interface. It is safe for concurrent file and chunk scheduling.
type NetworkHealthTracker struct {
	mu      sync.Mutex
	options NetworkHealthOptions
	states  map[int]*networkHealthState
	now     func() time.Time
}

// NewNetworkHealthTracker creates an empty interface-health tracker.
func NewNetworkHealthTracker(options NetworkHealthOptions) (*NetworkHealthTracker, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &NetworkHealthTracker{options: options, states: make(map[int]*networkHealthState), now: time.Now}, nil
}

// NewDefaultNetworkHealthTracker creates a tracker with P8 defaults.
func NewDefaultNetworkHealthTracker() *NetworkHealthTracker {
	tracker, _ := NewNetworkHealthTracker(DefaultNetworkHealthOptions())
	return tracker
}

// RecordSuccess immediately restores eligibility and resets failure backoff.
func (tracker *NetworkHealthTracker) RecordSuccess(path NetworkPath) {
	if tracker == nil || path.InterfaceIndex <= 0 {
		return
	}
	now := tracker.now()
	tracker.mu.Lock()
	state := tracker.stateLocked(path)
	state.successes++
	state.lastSuccess = now
	state.consecutiveFailures = 0
	state.cooldownUntil = time.Time{}
	state.score += networkHealthSuccessReward
	if state.score > defaultNetworkHealthScore {
		state.score = defaultNetworkHealthScore
	}
	tracker.mu.Unlock()
}

// RecordFailure lowers score and starts exponential cooldown for the interface.
func (tracker *NetworkHealthTracker) RecordFailure(path NetworkPath) {
	if tracker == nil || path.InterfaceIndex <= 0 {
		return
	}
	now := tracker.now()
	tracker.mu.Lock()
	state := tracker.stateLocked(path)
	state.failures++
	state.lastFailure = now
	state.consecutiveFailures++
	state.score -= networkHealthFailurePenalty
	if state.score < 0 {
		state.score = 0
	}
	state.cooldownUntil = now.Add(tracker.cooldownLocked(state.consecutiveFailures))
	tracker.mu.Unlock()
}

// Snapshot returns current health for path's physical interface.
func (tracker *NetworkHealthTracker) Snapshot(path NetworkPath) NetworkHealthSnapshot {
	if tracker == nil {
		return healthyNetworkSnapshot(path)
	}
	now := tracker.now()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	state, ok := tracker.states[path.InterfaceIndex]
	if !ok {
		return healthyNetworkSnapshot(path)
	}
	return NetworkHealthSnapshot{
		InterfaceName: state.interfaceName, InterfaceIndex: path.InterfaceIndex, Score: state.score,
		ConsecutiveFailures: state.consecutiveFailures, Successes: state.successes, Failures: state.failures,
		LastSuccess: state.lastSuccess, LastFailure: state.lastFailure, CooldownUntil: state.cooldownUntil,
		InCooldown: now.Before(state.cooldownUntil),
	}
}

// Available reports whether the interface is outside its cooldown window.
func (tracker *NetworkHealthTracker) Available(path NetworkPath) bool {
	return !tracker.Snapshot(path).InCooldown
}

// Score returns the interface health score used for dispatch preference.
func (tracker *NetworkHealthTracker) Score(path NetworkPath) int { return tracker.Snapshot(path).Score }

// NextAvailable returns the earliest active cooldown expiry among paths.
func (tracker *NetworkHealthTracker) NextAvailable(paths []NetworkPath) (time.Time, bool) {
	if tracker == nil {
		return time.Time{}, false
	}
	now := tracker.now()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	var earliest time.Time
	for _, path := range paths {
		state, ok := tracker.states[path.InterfaceIndex]
		if !ok || !now.Before(state.cooldownUntil) {
			continue
		}
		if earliest.IsZero() || state.cooldownUntil.Before(earliest) {
			earliest = state.cooldownUntil
		}
	}
	return earliest, !earliest.IsZero()
}

func (tracker *NetworkHealthTracker) stateLocked(path NetworkPath) *networkHealthState {
	state, ok := tracker.states[path.InterfaceIndex]
	if !ok {
		state = &networkHealthState{interfaceName: path.InterfaceName, score: defaultNetworkHealthScore}
		tracker.states[path.InterfaceIndex] = state
	} else if path.InterfaceName != "" {
		state.interfaceName = path.InterfaceName
	}
	return state
}

func (tracker *NetworkHealthTracker) cooldownLocked(consecutiveFailures int) time.Duration {
	cooldown := tracker.options.Cooldown
	for i := 1; i < consecutiveFailures && cooldown < tracker.options.CooldownMax; i++ {
		if cooldown > tracker.options.CooldownMax/2 {
			return tracker.options.CooldownMax
		}
		cooldown *= 2
	}
	if cooldown > tracker.options.CooldownMax {
		return tracker.options.CooldownMax
	}
	return cooldown
}

func healthyNetworkSnapshot(path NetworkPath) NetworkHealthSnapshot {
	return NetworkHealthSnapshot{InterfaceName: path.InterfaceName, InterfaceIndex: path.InterfaceIndex, Score: defaultNetworkHealthScore}
}

func waitForNetworkHealth(ctx context.Context, tracker *NetworkHealthTracker, paths []NetworkPath) (bool, error) {
	until, ok := tracker.NextAvailable(paths)
	if !ok {
		return false, nil
	}
	delay := time.Until(until)
	if tracker != nil && tracker.now != nil {
		delay = until.Sub(tracker.now())
	}
	if delay <= 0 {
		return true, nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func markNetworkPathFailure(err error) error {
	if err == nil || errors.Is(err, ErrNetworkPathFailure) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrNetworkPathFailure, err)
}

func markLikelyNetworkPathFailure(err error) error {
	if err == nil || errors.Is(err, ErrNetworkPathFailure) {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return markNetworkPathFailure(err)
	}
	return err
}

func shouldPenalizeNetworkPath(ctx context.Context, err error) bool {
	if err == nil || !errors.Is(err, ErrNetworkPathFailure) {
		return false
	}
	return ctx == nil || ctx.Err() == nil
}
