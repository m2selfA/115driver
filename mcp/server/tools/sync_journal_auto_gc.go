package tools

import "github.com/SheltonZhu/115driver/internal/transfer"

// runMCPSessionOpportunisticGC reuses the shared transfer SessionStore
// maintenance path for ordinary transfer sessions and the common trash/
// namespace (which also contains trashed sync journals). It intentionally does
// not garbage-collect current sync journals: those remain protected by the
// content-addressed plan_sync_journal_cleanup -> execute_sync_journal_cleanup
// review flow so private review aliases can be handled atomically.
func (ft *FileTools) runMCPSessionOpportunisticGC() error {
	if ft == nil || ft.syncJournalStore == nil {
		return nil
	}
	store := *ft.syncJournalStore
	if !store.AutoGC || store.GCInterval <= 0 {
		return nil
	}
	sessionStore := transfer.SessionStore{Root: store.Root}
	_, err := sessionStore.RunOpportunisticGC(store.GCInterval, transfer.SessionGCOptions{
		Retention:      store.Retention,
		TrashRetention: store.TrashRetention,
	})
	return err
}
