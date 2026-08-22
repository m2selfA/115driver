package syncguard

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/internal/remotetree"
	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func expectedSubtreeDescendants(plan syncplanpkg.Plan, rootItem syncplanpkg.Item, side syncexecpkg.SubtreeSide) ([]syncexecpkg.SubtreeNode, error) {
	expected, err := syncexecpkg.ExpectedSubtree(plan, rootItem.RelativePath, side)
	if err != nil {
		return nil, err
	}
	rootKey := syncplanpkg.PathKey(rootItem.RelativePath)
	descendants := make([]syncexecpkg.SubtreeNode, 0, len(expected)-1)
	for _, node := range expected {
		if syncplanpkg.PathKey(node.RelativePath) == rootKey {
			continue
		}
		descendants = append(descendants, node)
	}
	return descendants, nil
}

func remoteEntryRelativePath(rootItem syncplanpkg.Item, entry remotetree.Entry) (string, error) {
	rootPath := syncplanpkg.CanonicalRemoteRoot(rootItem.RemotePath)
	entryPath := syncplanpkg.CanonicalRemoteRoot(entry.RemotePath)
	prefix := strings.TrimRight(rootPath, "/") + "/"
	if rootPath == "/" {
		prefix = "/"
	}
	if entryPath == rootPath || !strings.HasPrefix(entryPath, prefix) {
		return "", fmt.Errorf("remote subtree walker returned path %q outside reviewed root %q", entry.RemotePath, rootItem.RemotePath)
	}
	suffix := strings.TrimPrefix(entryPath, prefix)
	return pathpkg.Join(rootItem.RelativePath, suffix), nil
}

func walkRemoteSubtree(client remotetree.Client, rootItem syncplanpkg.Item, visit func(remotetree.Entry) (bool, error)) error {
	if paged, ok := client.(remotetree.PagedClient); ok {
		_, err := remotetree.WalkPaged(paged, rootItem.RemoteID, rootItem.RemotePath, 0, visit)
		return err
	}
	_, err := remotetree.Walk(client, rootItem.RemoteID, rootItem.RemotePath, 0, visit)
	return err
}

// ValidateRemoteSubtree verifies all descendants below a planned remote
// directory using the canonical syncexec subtree comparator. The root itself is
// still validated by the caller before this guard is used for destruction.
func ValidateRemoteSubtree(client remotetree.Client, plan syncplanpkg.Plan, rootItem syncplanpkg.Item) error {
	expected, err := expectedSubtreeDescendants(plan, rootItem, syncexecpkg.SubtreeRemote)
	if err != nil {
		return fmt.Errorf("build reviewed remote subtree snapshot: %w", err)
	}
	actual := make([]syncexecpkg.SubtreeNode, 0, len(expected)+1)
	err = walkRemoteSubtree(client, rootItem, func(entry remotetree.Entry) (bool, error) {
		relativePath, err := remoteEntryRelativePath(rootItem, entry)
		if err != nil {
			return false, err
		}
		kind := "file"
		if entry.File.IsDirectory {
			kind = "directory"
		}
		modTime := int64(0)
		if !entry.File.UpdateTime.IsZero() {
			modTime = entry.File.UpdateTime.UnixNano()
		}
		actual = append(actual, syncexecpkg.SubtreeNode{
			RelativePath:    relativePath,
			Kind:            kind,
			Size:            entry.File.Size,
			ObjectID:        entry.File.FileID,
			SHA1:            strings.ToUpper(strings.TrimSpace(entry.File.Sha1)),
			ModTimeUnixNano: modTime,
		})
		return len(actual) > len(expected), nil
	})
	if err != nil {
		return err
	}
	if err := syncexecpkg.CompareSubtree(expected, actual); err != nil {
		return fmt.Errorf("remote replacement subtree %q changed after planning: %w", rootItem.RemotePath, err)
	}
	return nil
}

func localFileContentSnapshot(path string, before os.FileInfo) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open local replacement subtree file %q for content validation: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat local replacement subtree file %q before hashing: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return "", fmt.Errorf("local replacement subtree file %q changed identity or size before content validation", path)
	}
	h := sha1.New()
	written, err := io.Copy(h, file)
	if err != nil {
		return "", fmt.Errorf("hash local replacement subtree file %q: %w", path, err)
	}
	if written != before.Size() {
		return "", fmt.Errorf("local replacement subtree file %q changed size while hashing", path)
	}
	after, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("restat local replacement subtree file %q after hashing: %w", path, err)
	}
	pathAfter, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("restat local replacement subtree path %q after hashing: %w", path, err)
	}
	if pathAfter.Mode()&os.ModeSymlink != 0 || !pathAfter.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(before, pathAfter) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("local replacement subtree file %q changed while hashing", path)
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

var errLocalSubtreeCollectionBudget = errors.New("live local subtree exceeds reviewed snapshot budget")

func expectedLocalFileBytes(nodes []syncexecpkg.SubtreeNode) (int64, error) {
	var total int64
	for _, node := range nodes {
		if node.Kind != "file" {
			continue
		}
		if node.Size < 0 {
			return 0, fmt.Errorf("reviewed local subtree file %q has negative size", node.RelativePath)
		}
		if total > int64(^uint64(0)>>1)-node.Size {
			return 0, fmt.Errorf("reviewed local subtree byte total overflows int64")
		}
		total += node.Size
	}
	return total, nil
}

// ValidateLocalSubtree verifies all descendants below a planned local directory
// using the canonical syncexec subtree comparator. Live file content is hashed
// with before/after same-file checks so same-size/same-mtime rewrites fail.
func ValidateLocalSubtree(plan syncplanpkg.Plan, rootItem syncplanpkg.Item) error {
	rootPath, err := filepath.Abs(rootItem.LocalPath)
	if err != nil {
		return err
	}
	expected, err := expectedSubtreeDescendants(plan, rootItem, syncexecpkg.SubtreeLocal)
	if err != nil {
		return fmt.Errorf("build reviewed local subtree snapshot: %w", err)
	}
	maxFileBytes, err := expectedLocalFileBytes(expected)
	if err != nil {
		return err
	}
	var actualFileBytes int64
	actual := make([]syncexecpkg.SubtreeNode, 0, len(expected)+1)
	err = filepath.Walk(rootPath, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(rootPath, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		kind := "file"
		if info.IsDir() {
			kind = "directory"
		} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("local replacement subtree entry %q changed to an unsupported type", current)
		}
		node := syncexecpkg.SubtreeNode{
			RelativePath:    pathpkg.Join(rootItem.RelativePath, filepath.ToSlash(relative)),
			Kind:            kind,
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
		}
		if len(actual) >= len(expected) {
			actual = append(actual, node)
			return errLocalSubtreeCollectionBudget
		}
		if kind == "file" {
			if info.Size() < 0 || actualFileBytes > maxFileBytes-info.Size() {
				actual = append(actual, node)
				return errLocalSubtreeCollectionBudget
			}
			actualFileBytes += info.Size()
			node.SHA1, err = localFileContentSnapshot(current, info)
			if err != nil {
				return err
			}
		}
		actual = append(actual, node)
		return nil
	})
	if err != nil && !errors.Is(err, errLocalSubtreeCollectionBudget) {
		return err
	}
	if err := syncexecpkg.CompareSubtree(expected, actual); err != nil {
		return fmt.Errorf("local replacement subtree %q changed after planning: %w", rootItem.LocalPath, err)
	}
	return nil
}
