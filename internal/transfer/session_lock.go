package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrSessionLocked = errors.New("session is already in use")

type SessionLease struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Heartbeat time.Time `json:"heartbeat"`
}

type SessionLock struct {
	mu           sync.Mutex
	file         *os.File
	path         string
	leasePath    string
	stop         chan struct{}
	done         chan struct{}
	closed       bool
	leaseStopped bool
	lease        SessionLease
}

func AcquireSessionLock(lockPath, leasePath string) (*SessionLock, error) {
	lockPath = strings.TrimSpace(lockPath)
	if lockPath == "" {
		return nil, fmt.Errorf("%w: lock path is empty", ErrSessionStore)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, fmt.Errorf("create session lock directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := tryPlatformFileLock(file); err != nil {
		_ = file.Close()
		if isPlatformLockContention(err) {
			return nil, fmt.Errorf("%w: %s", ErrSessionLocked, filepath.Base(lockPath))
		}
		return nil, fmt.Errorf("acquire session lock: %w", err)
	}
	hostname, _ := os.Hostname()
	now := time.Now().UTC()
	lock := &SessionLock{
		file: file, path: lockPath, leasePath: strings.TrimSpace(leasePath),
		stop: make(chan struct{}), done: make(chan struct{}),
		lease: SessionLease{PID: os.Getpid(), Hostname: hostname, StartedAt: now, Heartbeat: now},
	}
	if lock.leasePath == "" {
		close(lock.done)
		return lock, nil
	}
	if err := lock.writeLease(); err != nil {
		_ = unlockPlatformFile(file)
		_ = file.Close()
		return nil, err
	}
	go lock.heartbeatLoop()
	return lock, nil
}

func (lock *SessionLock) StopLease() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	if lock.leasePath == "" || lock.leaseStopped {
		lock.mu.Unlock()
		return nil
	}
	lock.leaseStopped = true
	close(lock.stop)
	lock.mu.Unlock()
	<-lock.done
	err := os.Remove(lock.leasePath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (lock *SessionLock) Close() error {
	if lock == nil {
		return nil
	}
	leaseErr := lock.StopLease()
	lock.mu.Lock()
	if lock.closed {
		lock.mu.Unlock()
		return leaseErr
	}
	lock.closed = true
	file := lock.file
	lock.mu.Unlock()
	unlockErr := unlockPlatformFile(file)
	closeErr := file.Close()
	return errors.Join(leaseErr, unlockErr, closeErr)
}

func (lock *SessionLock) heartbeatLoop() {
	defer close(lock.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-lock.stop:
			return
		case <-ticker.C:
			lock.mu.Lock()
			if lock.closed {
				lock.mu.Unlock()
				return
			}
			lock.lease.Heartbeat = time.Now().UTC()
			_ = lock.writeLeaseLocked()
			lock.mu.Unlock()
		}
	}
}

func (lock *SessionLock) writeLease() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.writeLeaseLocked()
}

func (lock *SessionLock) writeLeaseLocked() error {
	if err := os.MkdirAll(filepath.Dir(lock.leasePath), 0700); err != nil {
		return fmt.Errorf("create session lease directory: %w", err)
	}
	encoded, err := json.MarshalIndent(lock.lease, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session lease: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(lock.leasePath), ".lease.json.*")
	if err != nil {
		return fmt.Errorf("create session lease temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write session lease: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync session lease: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceDownloadedFile(tmpPath, lock.leasePath); err != nil {
		return fmt.Errorf("replace session lease: %w", err)
	}
	cleanup = false
	return nil
}
