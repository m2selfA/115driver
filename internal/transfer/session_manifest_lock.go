package transfer

import (
	"path/filepath"
	"sync"
)

type sessionManifestLockEntry struct {
	mu   sync.Mutex
	refs int
}

var sessionManifestLockRegistry = struct {
	sync.Mutex
	entries map[string]*sessionManifestLockEntry
}{entries: make(map[string]*sessionManifestLockEntry)}

// lockSessionManifestPath serializes in-process read-modify-write cycles for one
// managed session manifest. The OS session lock prevents a second process from
// owning the same transfer, but recursive upload workers in the owner process
// can still touch the shared manifest through independent per-file resume state.
func lockSessionManifestPath(path string) func() {
	key := filepath.Clean(path)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	key = canonicalSessionLocalPath(key)

	sessionManifestLockRegistry.Lock()
	entry := sessionManifestLockRegistry.entries[key]
	if entry == nil {
		entry = &sessionManifestLockEntry{}
		sessionManifestLockRegistry.entries[key] = entry
	}
	entry.refs++
	sessionManifestLockRegistry.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		sessionManifestLockRegistry.Lock()
		entry.refs--
		if entry.refs == 0 && sessionManifestLockRegistry.entries[key] == entry {
			delete(sessionManifestLockRegistry.entries, key)
		}
		sessionManifestLockRegistry.Unlock()
	}
}
