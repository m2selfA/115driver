package cmd

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func deriveTransferSessionPaths(direction, localAnchor, remoteTarget, override string) (string, string, error) {
	localAbs, err := filepath.Abs(localAnchor)
	if err != nil {
		return "", "", fmt.Errorf("resolve transfer session anchor: %w", err)
	}
	var sessionPath string
	if strings.TrimSpace(override) != "" {
		sessionPath, err = filepath.Abs(strings.TrimSpace(override))
		if err != nil {
			return "", "", fmt.Errorf("resolve transfer session path: %w", err)
		}
	} else {
		base := safeTransferSessionBase(filepath.Base(localAbs))
		if base == "" {
			base = "transfer"
		}
		identityPath := canonicalTransferSessionLocalPath(localAbs)
		digest := sha256.Sum256([]byte(strings.Join([]string{direction, identityPath, remoteTarget}, "\x00")))
		shortHash := hex.EncodeToString(digest[:6])
		sessionPath = filepath.Join(filepath.Dir(localAbs), fmt.Sprintf(".%s.115driver-%s-%s.session.json", base, direction, shortHash))

		// Before Windows path identities were canonicalized, changing only path
		// casing could produce a second session file. Prefer the canonical name,
		// but keep discovering a legacy session created with the exact raw path.
		if identityPath != localAbs {
			if _, err := os.Lstat(sessionPath); os.IsNotExist(err) {
				legacyDigest := sha256.Sum256([]byte(strings.Join([]string{direction, localAbs, remoteTarget}, "\x00")))
				legacyHash := hex.EncodeToString(legacyDigest[:6])
				legacyPath := filepath.Join(filepath.Dir(localAbs), fmt.Sprintf(".%s.115driver-%s-%s.session.json", base, direction, legacyHash))
				if _, legacyErr := os.Lstat(legacyPath); legacyErr == nil {
					sessionPath = legacyPath
				}
			}
		}
	}
	return sessionPath, sessionPath + ".parts", nil
}

type transferSessionResolution struct {
	SessionPath    string
	PartsDir       string
	ManagedDir     string
	LegacyPath     string
	LegacyParts    string
	ImportedLegacy bool
	Lock           *transfer.SessionLock
}

func resolveTransferSessionPaths(direction, kind, localAnchor, remoteTarget, strategy, transferMode, override string) (transferSessionResolution, error) {
	if strings.TrimSpace(override) != "" {
		sessionPath, partsDir, err := deriveTransferSessionPaths(direction, localAnchor, remoteTarget, override)
		if err != nil {
			return transferSessionResolution{}, err
		}
		lock, err := transfer.AcquireSessionLock(sessionPath+".lock", "")
		if err != nil {
			return transferSessionResolution{}, err
		}
		return transferSessionResolution{SessionPath: sessionPath, PartsDir: partsDir, Lock: lock}, nil
	}
	legacyPath, legacyParts, err := deriveTransferSessionPaths(direction, localAnchor, remoteTarget, "")
	if err != nil {
		return transferSessionResolution{}, err
	}
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return transferSessionResolution{}, err
	}
	profileName := auth.ResolveProfileName(configPath, profile)
	profileScope, err := transfer.SessionProfileScope(auth.ResolveConfigFilePath(configPath), profileName)
	if err != nil {
		return transferSessionResolution{}, err
	}
	identity, err := transfer.NewSessionIdentityV2(direction, kind, profileScope, localAnchor, remoteTarget, strategy, transferMode)
	if err != nil {
		return transferSessionResolution{}, err
	}
	localAbs, err := filepath.Abs(localAnchor)
	if err != nil {
		return transferSessionResolution{}, err
	}
	accountID := int64(0)
	if client != nil {
		accountID = client.UserID
	}
	store := transfer.SessionStore{Root: config.SessionDir}
	location, err := store.Location(identity, filepath.Base(localAbs))
	if err != nil {
		return transferSessionResolution{}, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return transferSessionResolution{}, err
	}
	location, _, err = store.Open(identity, filepath.Base(localAbs), accountID)
	if errors.Is(err, transfer.ErrSessionCompleted) {
		if leaseErr := lock.StopLease(); leaseErr != nil {
			_ = lock.Close()
			return transferSessionResolution{}, leaseErr
		}
		if _, cleanupErr := transfer.RemoveManagedSessionForPayload(location.PayloadPath); cleanupErr != nil {
			_ = lock.Close()
			return transferSessionResolution{}, cleanupErr
		}
		location, _, err = store.Open(identity, filepath.Base(localAbs), accountID)
	}
	if errors.Is(err, transfer.ErrSessionStore) {
		if leaseErr := lock.StopLease(); leaseErr != nil {
			_ = lock.Close()
			return transferSessionResolution{}, leaseErr
		}
		if _, quarantineErr := store.QuarantineCorruptLocation(location); quarantineErr != nil {
			_ = lock.Close()
			return transferSessionResolution{}, quarantineErr
		}
		location, _, err = store.Open(identity, filepath.Base(localAbs), accountID)
	}
	if err != nil {
		_ = lock.Close()
		return transferSessionResolution{}, err
	}
	if config.SessionAutoGC {
		if _, gcErr := store.RunOpportunisticGC(config.SessionGCInterval, transfer.SessionGCOptions{
			Retention: config.SessionRetention, TrashRetention: config.SessionTrashRetention,
		}); gcErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: session GC failed: %v\n", gcErr)
		}
	}
	return transferSessionResolution{
		SessionPath: location.PayloadPath, PartsDir: location.PartsDir, ManagedDir: location.Dir,
		LegacyPath: legacyPath, LegacyParts: legacyParts, Lock: lock,
	}, nil
}

func legacyTransferSessionImportNeeded(resolution transferSessionResolution) (bool, error) {
	if resolution.ManagedDir == "" || strings.TrimSpace(resolution.LegacyPath) == "" {
		return false, nil
	}
	if _, err := os.Lstat(resolution.SessionPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	info, err := os.Lstat(resolution.LegacyPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0, nil
}

func importLegacyTransferSession(resolution *transferSessionResolution, validate func(string) (bool, error)) error {
	if resolution == nil || resolution.ManagedDir == "" || strings.TrimSpace(resolution.LegacyPath) == "" || validate == nil {
		return nil
	}
	if _, err := os.Lstat(resolution.SessionPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	valid, err := validate(resolution.LegacyPath)
	if err != nil {
		return err
	}
	if !valid {
		return nil
	}
	imported, err := transfer.ImportLegacySession(transfer.SessionLocation{
		Dir: resolution.ManagedDir, PayloadPath: resolution.SessionPath, PartsDir: resolution.PartsDir,
	}, resolution.LegacyPath, resolution.LegacyParts)
	if err != nil || !imported {
		return err
	}
	copiedValid, verifyErr := validate(resolution.SessionPath)
	if verifyErr != nil || !copiedValid {
		removePayloadErr := os.Remove(resolution.SessionPath)
		if os.IsNotExist(removePayloadErr) {
			removePayloadErr = nil
		}
		removePartsErr := os.RemoveAll(resolution.PartsDir)
		if verifyErr == nil {
			verifyErr = fmt.Errorf("imported legacy session failed destination validation")
		}
		return errors.Join(verifyErr, removePayloadErr, removePartsErr)
	}
	resolution.ImportedLegacy = true
	return nil
}

func (resolution *transferSessionResolution) closeLock() error {
	if resolution == nil || resolution.Lock == nil {
		return nil
	}
	err := resolution.Lock.Close()
	resolution.Lock = nil
	return err
}

func commitLegacyTransferSessionImport(resolution transferSessionResolution) {
	if resolution.ImportedLegacy {
		transfer.RemoveLegacySessionBestEffort(resolution.LegacyPath, resolution.LegacyParts)
	}
}

func cleanupResolvedTransferSession(resolution transferSessionResolution) error {
	if resolution.ManagedDir == "" {
		return nil
	}
	if resolution.Lock != nil {
		if err := resolution.Lock.StopLease(); err != nil {
			return err
		}
	}
	_, err := transfer.RemoveManagedSessionForPayload(resolution.SessionPath)
	return err
}

func canonicalTransferSessionLocalPath(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func uploadResumePathForRelative(partsDir, relative string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(relative)))
	return filepath.Join(partsDir, hex.EncodeToString(digest[:12])+".upload.json")
}

func downloadResumePathForRelative(partsDir, relative string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(relative)))
	return filepath.Join(partsDir, hex.EncodeToString(digest[:12])+".download.json")
}

func safeTransferSessionBase(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), ".")
}

func pathIsWithin(root, target string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func completedDownloadFileStillValid(root string, file transfer.TransferTreeSessionFile) (bool, int64, error) {
	destination := filepath.Join(root, file.RelativePath)
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != file.Size {
		return false, 0, nil
	}
	if file.LocalModTimeUnixNano != 0 && info.ModTime().UnixNano() == file.LocalModTimeUnixNano {
		return true, info.ModTime().UnixNano(), nil
	}
	if strings.TrimSpace(file.SHA1) == "" {
		return false, info.ModTime().UnixNano(), nil
	}
	matches, err := fileSHA1Matches(destination, file.SHA1)
	return matches, info.ModTime().UnixNano(), err
}

func fileSHA1Matches(path, expected string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha1.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false, err
	}
	actual := strings.ToUpper(hex.EncodeToString(hash.Sum(nil)))
	return strings.EqualFold(actual, strings.TrimSpace(expected)), nil
}
