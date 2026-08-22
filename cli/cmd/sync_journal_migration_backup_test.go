package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func legacyMigrationBackupRecord(source []byte) syncJournalMigrationRecord {
	digest := sha256.Sum256(source)
	return syncJournalMigrationRecord{
		FromVersion: 1, ToVersion: 2, SourceSHA256: hex.EncodeToString(digest[:]), BackupRequired: true,
	}
}

func prepareLegacyMigrationForTOCTOUTest(t *testing.T, store syncJournalStore) (syncJournalPreparedMigration, syncJournalLocation, []byte, *transfer.SessionLock) {
	t.Helper()
	plan := testSyncJournalPlan(t)
	location, original := writeLegacySyncJournalV1(t, store, plan, nil)
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.prepareMigrationLocked(location)
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := store.ensurePreparedMigrationBackups(prepared); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	return prepared, location, original, lock
}

func TestPreparedMigrationRejectsSourceBytesChangedAfterPrepare(t *testing.T) {
	store := testSyncJournalStore(t)
	prepared, location, original, lock := prepareLegacyMigrationForTOCTOUTest(t, store)
	defer lock.Close()
	tampered := append(append([]byte(nil), original...), '\n')
	if err := os.WriteFile(location.JournalPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.persistPreparedMigrationJournal(prepared); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "changed after migration preparation") {
		t.Fatalf("prepared migration overwrote changed source bytes: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, tampered) {
		t.Fatal("source revalidation failure overwrote externally changed journal")
	}
}

func TestPreparedMigrationRollbackRejectsUnknownBytesAndPreservesThem(t *testing.T) {
	store := testSyncJournalStore(t)
	prepared, location, _, lock := prepareLegacyMigrationForTOCTOUTest(t, store)
	defer lock.Close()
	if err := store.persistPreparedMigrationJournal(prepared); err != nil {
		t.Fatal(err)
	}
	tampered := append(append([]byte(nil), prepared.Upgraded...), '\n')
	if err := os.WriteFile(location.JournalPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.rollbackPreparedMigration(prepared); !errors.Is(err, errSyncJournalMigrationBatchRecoveryRequired) || !strings.Contains(err.Error(), "unknown bytes") {
		t.Fatalf("rollback overwrote unknown external bytes: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, tampered) {
		t.Fatal("rollback revalidation failure changed unknown journal bytes")
	}
}

func TestPreparedMigrationRollbackIsIdempotentAtExactSource(t *testing.T) {
	store := testSyncJournalStore(t)
	prepared, location, original, lock := prepareLegacyMigrationForTOCTOUTest(t, store)
	defer lock.Close()
	if err := store.persistPreparedMigrationJournal(prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.rollbackPreparedMigration(prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.rollbackPreparedMigration(prepared); err != nil {
		t.Fatalf("second exact-source rollback was not idempotent: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("idempotent rollback changed exact source bytes")
	}
}

func TestSyncJournalMigrationBackupNoClobberOnDifferentExistingBytes(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, original := writeLegacySyncJournalV1(t, store, plan, nil)
	record := legacyMigrationBackupRecord(original)
	backupPath, err := syncJournalMigrationBackupPath(location, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
		t.Fatal(err)
	}
	wrong := []byte("not-the-source-journal")
	if err := os.WriteFile(backupPath, wrong, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Migrate(plan.PlanID); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "different contents") {
		t.Fatalf("migration overwrote conflicting backup or returned wrong error: %v", err)
	}
	journalAfter, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journalAfter, original) {
		t.Fatal("conflicting migration backup changed the source journal")
	}
	backupAfter, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupAfter, wrong) {
		t.Fatal("conflicting migration backup was overwritten")
	}
}

func TestSyncJournalMigrationBackupExactExistingBytesAreIdempotent(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, original := writeLegacySyncJournalV1(t, store, plan, nil)
	record := legacyMigrationBackupRecord(original)
	backupPath, err := syncJournalMigrationBackupPath(location, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, original, 0600); err != nil {
		t.Fatal(err)
	}

	result, err := store.Migrate(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated {
		t.Fatalf("migration with exact existing backup did not run: %#v", result)
	}
	backupAfter, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupAfter, original) {
		t.Fatal("idempotent migration backup changed exact existing bytes")
	}
}

func TestSyncJournalMigrationBackupDirectoryFailureLeavesJournalUntouched(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, original := writeLegacySyncJournalV1(t, store, plan, nil)
	blocker := filepath.Join(location.Dir, syncJournalMigrationBackupDirName)
	if err := os.WriteFile(blocker, []byte("block-directory"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Migrate(plan.PlanID); err == nil || !strings.Contains(err.Error(), "migration backup") {
		t.Fatalf("backup directory failure did not block migration: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("backup directory failure changed the source journal")
	}
}
