package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SessionMaintenance struct {
	LastGC time.Time `json:"last_gc"`
}

func (store SessionStore) RunGCExclusive(options SessionGCOptions) ([]SessionGCAction, error) {
	root, err := store.absoluteRoot()
	if err != nil {
		return nil, err
	}
	gcLock, err := AcquireSessionLock(filepath.Join(root, "gc.lock"), "")
	if err != nil {
		return nil, err
	}
	defer gcLock.Close()
	return store.GC(options)
}

func (store SessionStore) RunOpportunisticGC(interval time.Duration, options SessionGCOptions) ([]SessionGCAction, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("%w: GC interval must be > 0", ErrSessionStore)
	}
	root, err := store.absoluteRoot()
	if err != nil {
		return nil, err
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
		options.Now = now
	}
	maintenancePath := filepath.Join(root, "maintenance.json")
	maintenance, err := readSessionMaintenance(maintenancePath)
	if err != nil {
		return nil, err
	}
	if !maintenance.LastGC.IsZero() && now.Sub(maintenance.LastGC) < interval {
		return nil, nil
	}
	gcLock, err := AcquireSessionLock(filepath.Join(root, "gc.lock"), "")
	if errors.Is(err, ErrSessionLocked) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer gcLock.Close()

	maintenance, err = readSessionMaintenance(maintenancePath)
	if err != nil {
		return nil, err
	}
	if !maintenance.LastGC.IsZero() && now.Sub(maintenance.LastGC) < interval {
		return nil, nil
	}
	actions, err := store.GC(options)
	if err != nil {
		return actions, err
	}
	if options.DryRun {
		return actions, nil
	}
	maintenance.LastGC = now
	if err := writeSessionMaintenanceAtomic(maintenancePath, maintenance); err != nil {
		return actions, err
	}
	return actions, nil
}

func readSessionMaintenance(path string) (SessionMaintenance, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return SessionMaintenance{}, nil
	}
	if err != nil {
		return SessionMaintenance{}, fmt.Errorf("read session maintenance: %w", err)
	}
	var maintenance SessionMaintenance
	if err := json.Unmarshal(data, &maintenance); err != nil {
		return SessionMaintenance{}, fmt.Errorf("decode session maintenance: %w", err)
	}
	return maintenance, nil
}

func writeSessionMaintenanceAtomic(path string, maintenance SessionMaintenance) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(maintenance, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".maintenance.json.*")
	if err != nil {
		return err
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
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceDownloadedFile(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
