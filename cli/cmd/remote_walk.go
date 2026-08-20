package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type remoteTreeListClient interface {
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
}

type remoteWalkEntry struct {
	File         driver.File
	RelativePath string
	RemotePath   string
	ParentPath   string
	Depth        int
}

type remoteWalkResult struct {
	StoppedEarly bool
	DepthLimited bool
}

func walkRemoteTree(client remoteTreeListClient, rootID, rootPath string, maxDepth int, visit func(remoteWalkEntry) (bool, error)) (remoteWalkResult, error) {
	result := remoteWalkResult{}
	if client == nil {
		return result, errors.New("remote tree client is nil")
	}
	if strings.TrimSpace(rootID) == "" {
		return result, errors.New("remote tree root ID is empty")
	}
	if maxDepth < 0 {
		return result, errors.New("max depth must be >= 0")
	}
	if visit == nil {
		return result, errors.New("remote tree visitor is nil")
	}
	if rootPath == "" {
		rootPath = "/"
	}

	type pendingDirectory struct {
		ID       string
		Relative string
		Remote   string
		Depth    int
	}
	queue := []pendingDirectory{{ID: rootID, Remote: rootPath}}
	seenDirectories := map[string]struct{}{rootID: {}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		entries, err := client.List(current.ID)
		if err != nil {
			return result, fmt.Errorf("list remote directory %q: %w", current.Remote, err)
		}
		for _, entry := range *entries {
			depth := current.Depth + 1
			relative := entry.Name
			if current.Relative != "" {
				relative = current.Relative + "/" + entry.Name
			}
			remotePath := joinRemoteDisplayPath(current.Remote, entry.Name)
			stop, err := visit(remoteWalkEntry{
				File: entry, RelativePath: relative, RemotePath: remotePath, ParentPath: current.Remote, Depth: depth,
			})
			if err != nil {
				return result, err
			}
			if stop {
				result.StoppedEarly = true
				return result, nil
			}
			if !entry.IsDirectory {
				continue
			}
			if maxDepth > 0 && depth >= maxDepth {
				result.DepthLimited = true
				continue
			}
			if strings.TrimSpace(entry.FileID) == "" {
				return result, fmt.Errorf("remote directory %q has no stable ID", remotePath)
			}
			if _, exists := seenDirectories[entry.FileID]; exists {
				return result, fmt.Errorf("remote directory ID %q was encountered more than once", entry.FileID)
			}
			seenDirectories[entry.FileID] = struct{}{}
			queue = append(queue, pendingDirectory{ID: entry.FileID, Relative: relative, Remote: remotePath, Depth: depth})
		}
	}
	return result, nil
}

func joinRemoteDisplayPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return "/" + strings.TrimPrefix(name, "/")
	}
	return strings.TrimRight(parent, "/") + "/" + strings.TrimPrefix(name, "/")
}
