package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const syncJournalMigrationBackupDirName = "migration-backups"

func syncJournalMigrationBackupPath(location syncJournalLocation, record syncJournalMigrationRecord) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(record.SourceSHA256))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%w: invalid migration backup source SHA-256", errSyncJournalInvalidSchema)
	}
	if record.FromVersion < syncJournalMinReadableVersion || record.ToVersion != record.FromVersion+1 {
		return "", fmt.Errorf("%w: invalid migration backup version edge %d -> %d", errSyncJournalInvalidSchema, record.FromVersion, record.ToVersion)
	}
	return filepath.Join(location.Dir, syncJournalMigrationBackupDirName, fmt.Sprintf("v%d-%s.json", record.FromVersion, digest)), nil
}

func validateSyncJournalMigrationBackupBytes(record syncJournalMigrationRecord, data []byte) error {
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	if !strings.EqualFold(actual, strings.TrimSpace(record.SourceSHA256)) {
		return fmt.Errorf("%w: migration backup SHA-256 mismatch: expected %s got %s", errSyncJournalInvalidSchema, record.SourceSHA256, actual)
	}
	return nil
}

func readSyncJournalMigrationBackup(location syncJournalLocation, record syncJournalMigrationRecord) ([]byte, error) {
	path, err := syncJournalMigrationBackupPath(location, record)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := validateSyncJournalMigrationBackupBytes(record, data); err != nil {
		return nil, err
	}
	return data, nil
}

func ensureSyncJournalMigrationBackup(location syncJournalLocation, record syncJournalMigrationRecord, source []byte) error {
	if err := validateSyncJournalMigrationBackupBytes(record, source); err != nil {
		return err
	}
	path, err := syncJournalMigrationBackupPath(location, record)
	if err != nil {
		return err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(existing, source) {
			return fmt.Errorf("%w: migration backup %q already exists with different contents", errSyncJournalInvalidSchema, path)
		}
		return nil
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create migration backup directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			existing, readErr := os.ReadFile(path)
			if readErr == nil && bytes.Equal(existing, source) {
				return nil
			}
		}
		return fmt.Errorf("create migration backup: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure migration backup: %w", err)
	}
	if _, err := file.Write(source); err != nil {
		_ = file.Close()
		return fmt.Errorf("write migration backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync migration backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close migration backup: %w", err)
	}
	cleanup = false
	return nil
}
