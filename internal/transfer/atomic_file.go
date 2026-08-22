package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WritePrivateFileAtomic replaces path with data using a 0600 temporary file in
// the same directory. The destination directory is created with mode 0700.
func WritePrivateFileAtomic(path string, data []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("atomic private file path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create atomic private file directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".atomic-private.*")
	if err != nil {
		return fmt.Errorf("create atomic private file temp: %w", err)
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
		return fmt.Errorf("secure atomic private file temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write atomic private file temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync atomic private file temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close atomic private file temp: %w", err)
	}
	if err := replaceDownloadedFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace atomic private file: %w", err)
	}
	cleanup = false
	return nil
}
