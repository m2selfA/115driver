package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

type syncJournalMaintenance struct {
	LastGC time.Time `json:"last_gc"`
}

func (store syncJournalStore) RunGCExclusive(olderThan time.Duration, dryRun bool) ([]syncJournalGCAction, error) {
	root, err := store.root()
	if err != nil {
		return nil, err
	}
	lock, err := transfer.AcquireSessionLock(filepath.Join(root, "gc.lock"), "")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	return store.GC(olderThan, dryRun)
}

func (store syncJournalStore) RunOpportunisticGC(interval time.Duration) ([]syncJournalGCAction, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("sync journal GC interval must be > 0")
	}
	root, err := store.root()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	maintenancePath := filepath.Join(root, "maintenance.json")
	maintenance, err := readSyncJournalMaintenance(maintenancePath)
	if err != nil {
		return nil, err
	}
	if !maintenance.LastGC.IsZero() && now.Sub(maintenance.LastGC) < interval {
		return nil, nil
	}
	lock, err := transfer.AcquireSessionLock(filepath.Join(root, "gc.lock"), "")
	if errors.Is(err, transfer.ErrSessionLocked) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	maintenance, err = readSyncJournalMaintenance(maintenancePath)
	if err != nil {
		return nil, err
	}
	if !maintenance.LastGC.IsZero() && now.Sub(maintenance.LastGC) < interval {
		return nil, nil
	}
	actions, err := store.GC(0, false)
	if err != nil {
		return actions, err
	}
	maintenance.LastGC = now
	encoded, err := json.MarshalIndent(maintenance, "", "  ")
	if err != nil {
		return actions, err
	}
	if err := transfer.WritePrivateFileAtomic(maintenancePath, encoded); err != nil {
		return actions, err
	}
	return actions, nil
}

func runSyncJournalOpportunisticMaintenance(store syncJournalStore) error {
	if !store.AutoGC {
		return nil
	}
	_, sessionErr := (transfer.SessionStore{Root: store.Root}).RunOpportunisticGC(store.GCInterval, transfer.SessionGCOptions{
		Retention: store.Retention, TrashRetention: store.TrashRetention,
	})
	_, journalErr := store.RunOpportunisticGC(store.GCInterval)
	return errors.Join(sessionErr, journalErr)
}

func readSyncJournalMaintenance(path string) (syncJournalMaintenance, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return syncJournalMaintenance{}, nil
	}
	if err != nil {
		return syncJournalMaintenance{}, fmt.Errorf("read sync journal maintenance: %w", err)
	}
	var maintenance syncJournalMaintenance
	if err := json.Unmarshal(data, &maintenance); err != nil {
		return syncJournalMaintenance{}, fmt.Errorf("decode sync journal maintenance: %w", err)
	}
	return maintenance, nil
}
