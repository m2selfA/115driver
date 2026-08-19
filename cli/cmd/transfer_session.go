package cmd

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

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
		digest := sha256.Sum256([]byte(strings.Join([]string{direction, localAbs, remoteTarget}, "\x00")))
		shortHash := hex.EncodeToString(digest[:6])
		sessionPath = filepath.Join(filepath.Dir(localAbs), fmt.Sprintf(".%s.115driver-%s-%s.session.json", base, direction, shortHash))
	}
	return sessionPath, sessionPath + ".parts", nil
}

func uploadResumePathForRelative(partsDir, relative string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(relative)))
	return filepath.Join(partsDir, hex.EncodeToString(digest[:12])+".upload.json")
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
