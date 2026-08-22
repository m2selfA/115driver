package syncplan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

// IsTransferStateName identifies 115driver transfer/session artifacts that are
// implementation state rather than user data. Recursive upload and sync scans
// must share this rule so a planner never proposes copying its own journal.
func IsTransferStateName(name string, isDir bool) bool {
	if !strings.Contains(name, ".115driver-") {
		return false
	}
	if isDir {
		return strings.HasSuffix(name, ".session.json.parts")
	}
	if strings.HasSuffix(name, ".session.json") {
		return true
	}
	return strings.Contains(name, ".session.json.")
}

// LocalSnapshot is a bounded scan of one real local directory. Root is the
// absolute source directory; Entries excludes the root itself.
type LocalSnapshot struct {
	Root    string
	Entries map[string]Entry
	Nodes   int
}

// ScanLocal builds the same local sync domain consumed by Build. It rejects
// symlinks/special files, omits 115driver transfer state, and fails rather than
// returning a partial snapshot when maxNodes is exceeded.
func ScanLocal(ctx context.Context, root, remoteRoot string, maxNodes int) (LocalSnapshot, error) {
	if maxNodes < 0 {
		return LocalSnapshot{}, fmt.Errorf("max_nodes must be >= 0")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return LocalSnapshot{}, err
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil {
		return LocalSnapshot{}, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return LocalSnapshot{}, fmt.Errorf("sync source must be a real directory")
	}
	remoteRoot = CanonicalRemoteRoot(remoteRoot)
	snapshot := LocalSnapshot{Root: absolute, Entries: make(map[string]Entry)}
	err = filepath.WalkDir(absolute, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if path == absolute {
			return nil
		}
		relativeOS, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		if dirEntry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link %q is not allowed in sync source", relativeOS)
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		if IsTransferStateName(dirEntry.Name(), info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("special file %q is not supported", relativeOS)
		}
		if snapshot.Nodes >= maxNodes {
			return fmt.Errorf("local sync tree exceeds max_nodes budget %d", maxNodes)
		}
		relative := filepath.ToSlash(relativeOS)
		if err := ValidateRelativePath(relative); err != nil {
			return fmt.Errorf("invalid local sync path %q: %w", relative, err)
		}
		kind := "file"
		size := info.Size()
		if info.IsDir() {
			kind = "directory"
			size = 0
		}
		entry := Entry{
			RelativePath:    relative,
			Kind:            kind,
			LocalPath:       path,
			RemotePath:      RemoteChildPath(remoteRoot, relative),
			Size:            size,
			ModTimeUnixNano: info.ModTime().UnixNano(),
		}
		if err := AddEntry(snapshot.Entries, entry, "local"); err != nil {
			return err
		}
		snapshot.Nodes++
		return nil
	})
	if err != nil {
		return LocalSnapshot{}, err
	}
	return snapshot, nil
}

// PrepareLocalDigest validates the scan-time local file identity, computes the
// same upload digest used by execution, and verifies that the file did not
// change while hashing. The returned digest can be retained by a planner for
// later execution reuse without re-hashing the file.
func PrepareLocalDigest(local Entry) (*uploadpkg.PreparedDigest, error) {
	info, err := os.Lstat(local.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("inspect local file %q before sync checksum: %w", local.LocalPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != local.Size || info.ModTime().UnixNano() != local.ModTimeUnixNano {
		return nil, fmt.Errorf("local file %q changed after the sync tree was scanned", local.LocalPath)
	}
	file, err := os.Open(local.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("open local file %q for sync checksum: %w", local.LocalPath, err)
	}
	defer file.Close()
	digest, err := uploadpkg.PrepareFileDigest(file, local.Size)
	if err != nil {
		return nil, fmt.Errorf("checksum local file %q: %w", local.LocalPath, err)
	}
	if digest.ModTimeUnixNano != local.ModTimeUnixNano {
		return nil, fmt.Errorf("local file %q changed after the sync tree was scanned", local.LocalPath)
	}
	return digest, nil
}
