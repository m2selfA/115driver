package upload

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	hash "github.com/SheltonZhu/115driver/pkg/crypto"
)

// PreparedDigest contains the upload identity that 115 rapid-upload needs.
// Callers may compute it once for an existing-target verification and pass it
// through Options so the upload path does not hash the same file again.
type PreparedDigest struct {
	Size            int64
	ModTimeUnixNano int64
	PreID           string
	SHA1            string
}

// PrepareFileDigest hashes the file from the beginning, validates the observed
// size, and rewinds it so the same handle is ready for rapid-upload or OSS.
func PrepareFileDigest(file *os.File, expectedSize int64) (*PreparedDigest, error) {
	if file == nil {
		return nil, errors.New("upload file is nil")
	}
	if expectedSize < 0 {
		return nil, errors.New("upload file size must be >= 0")
	}
	before, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat upload file before digest: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() != expectedSize {
		return nil, fmt.Errorf("upload file changed before preparation: expected size=%d actual size=%d", expectedSize, before.Size())
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek upload file before digest: %w", err)
	}
	var digest hash.DigestResult
	digestErr := hash.Digest(file, &digest)
	after, statErr := file.Stat()
	_, seekErr := file.Seek(0, io.SeekStart)
	if statErr != nil {
		return nil, fmt.Errorf("stat upload file after digest: %w", statErr)
	}
	if after.Size() != before.Size() || after.ModTime().UnixNano() != before.ModTime().UnixNano() {
		return nil, errors.New("upload file changed while calculating digest")
	}
	if digestErr != nil {
		if seekErr != nil {
			return nil, errors.Join(fmt.Errorf("calculate upload digest: %w", digestErr), fmt.Errorf("rewind upload file after digest: %w", seekErr))
		}
		return nil, fmt.Errorf("calculate upload digest: %w", digestErr)
	}
	if seekErr != nil {
		return nil, fmt.Errorf("rewind upload file after digest: %w", seekErr)
	}
	if digest.Size != expectedSize {
		return nil, fmt.Errorf("upload file size changed during preparation: stat=%d digest=%d", expectedSize, digest.Size)
	}
	return &PreparedDigest{Size: digest.Size, ModTimeUnixNano: after.ModTime().UnixNano(), PreID: digest.PreID, SHA1: digest.QuickID}, nil
}

func resolveUploadDigest(file *os.File, expectedSize int64, prepared *PreparedDigest) (*PreparedDigest, error) {
	if prepared == nil {
		return PrepareFileDigest(file, expectedSize)
	}
	if file == nil {
		return nil, errors.New("upload file is nil")
	}
	if prepared.Size != expectedSize {
		return nil, fmt.Errorf("prepared upload digest size %d does not match file size %d", prepared.Size, expectedSize)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat upload file for prepared digest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != prepared.Size || info.ModTime().UnixNano() != prepared.ModTimeUnixNano {
		return nil, errors.New("upload file changed after digest preparation")
	}
	if strings.TrimSpace(prepared.PreID) == "" || strings.TrimSpace(prepared.SHA1) == "" {
		return nil, errors.New("prepared upload digest is incomplete")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek upload file for prepared digest: %w", err)
	}
	copy := *prepared
	return &copy, nil
}
