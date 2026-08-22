package syncexec

import (
	"fmt"
	"sort"
	"strings"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

type SubtreeSide string

const (
	SubtreeLocal  SubtreeSide = "local"
	SubtreeRemote SubtreeSide = "remote"
)

type SubtreeNode struct {
	RelativePath    string
	Kind            string
	Size            int64
	ObjectID        string
	SHA1            string
	ModTimeUnixNano int64
}

func ExpectedSubtree(plan syncplanpkg.Plan, rootRelativePath string, side SubtreeSide) ([]SubtreeNode, error) {
	rootKey := syncplanpkg.PathKey(rootRelativePath)
	if rootKey == "" {
		return nil, fmt.Errorf("subtree root is required")
	}
	if side != SubtreeLocal && side != SubtreeRemote {
		return nil, fmt.Errorf("unsupported subtree side %q", side)
	}
	result := make([]SubtreeNode, 0)
	for _, item := range plan.Items {
		key := syncplanpkg.PathKey(item.RelativePath)
		if key != rootKey && !strings.HasPrefix(key, rootKey+"/") {
			continue
		}
		node, ok, err := expectedSubtreeNode(item, side)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, node)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("reviewed plan has no %s subtree at %q", side, rootRelativePath)
	}
	sortSubtreeNodes(result)
	if syncplanpkg.PathKey(result[0].RelativePath) != rootKey {
		return nil, fmt.Errorf("reviewed %s subtree root %q is missing", side, rootRelativePath)
	}
	seen := make(map[string]struct{}, len(result))
	for _, node := range result {
		key := syncplanpkg.PathKey(node.RelativePath)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("reviewed %s subtree contains duplicate path %q", side, node.RelativePath)
		}
		seen[key] = struct{}{}
		if node.Kind != "file" && node.Kind != "directory" {
			return nil, fmt.Errorf("reviewed %s subtree path %q has unsupported kind %q", side, node.RelativePath, node.Kind)
		}
		if node.Kind == "file" && strings.TrimSpace(node.SHA1) == "" {
			return nil, fmt.Errorf("reviewed %s subtree file %q has no content snapshot", side, node.RelativePath)
		}
		if side == SubtreeRemote && strings.TrimSpace(node.ObjectID) == "" {
			return nil, fmt.Errorf("reviewed remote subtree path %q has no object identity", node.RelativePath)
		}
	}
	return result, nil
}

func expectedSubtreeNode(item syncplanpkg.Item, side SubtreeSide) (SubtreeNode, bool, error) {
	switch side {
	case SubtreeLocal:
		if !item.LocalPresent {
			return SubtreeNode{}, false, nil
		}
		kind := item.Kind
		if item.Action == "replace-local" && item.ReplacesKind != "" {
			kind = item.ReplacesKind
		}
		return SubtreeNode{
			RelativePath:    item.RelativePath,
			Kind:            kind,
			Size:            item.LocalSize,
			SHA1:            strings.ToUpper(strings.TrimSpace(item.LocalSHA1)),
			ModTimeUnixNano: item.LocalModTimeUnixNano,
		}, true, nil
	case SubtreeRemote:
		if !item.RemotePresent {
			return SubtreeNode{}, false, nil
		}
		kind := item.Kind
		if item.Action == "replace-remote" && item.ReplacesKind != "" {
			kind = item.ReplacesKind
		}
		return SubtreeNode{
			RelativePath:    item.RelativePath,
			Kind:            kind,
			Size:            item.RemoteSize,
			ObjectID:        item.RemoteID,
			SHA1:            strings.ToUpper(strings.TrimSpace(item.RemoteSHA1)),
			ModTimeUnixNano: item.RemoteModTimeUnixNano,
		}, true, nil
	default:
		return SubtreeNode{}, false, fmt.Errorf("unsupported subtree side %q", side)
	}
}

func CompareSubtree(expected, actual []SubtreeNode) error {
	left := append([]SubtreeNode(nil), expected...)
	right := append([]SubtreeNode(nil), actual...)
	sortSubtreeNodes(left)
	sortSubtreeNodes(right)
	if len(left) != len(right) {
		return fmt.Errorf("subtree node count changed: expected %d, got %d", len(left), len(right))
	}
	for i := range left {
		expectedNode := normalizeSubtreeNode(left[i])
		actualNode := normalizeSubtreeNode(right[i])
		if syncplanpkg.PathKey(expectedNode.RelativePath) != syncplanpkg.PathKey(actualNode.RelativePath) {
			return fmt.Errorf("subtree path set changed")
		}
		if expectedNode.Kind != actualNode.Kind {
			return fmt.Errorf("subtree path %q changed type", expectedNode.RelativePath)
		}
		if expectedNode.Kind == "file" {
			if expectedNode.Size != actualNode.Size {
				return fmt.Errorf("subtree file %q changed size", expectedNode.RelativePath)
			}
			if expectedNode.SHA1 == "" || actualNode.SHA1 == "" || !strings.EqualFold(expectedNode.SHA1, actualNode.SHA1) {
				return fmt.Errorf("subtree file %q changed content", expectedNode.RelativePath)
			}
		}
		if expectedNode.ObjectID != "" && expectedNode.ObjectID != actualNode.ObjectID {
			return fmt.Errorf("subtree path %q changed object identity", expectedNode.RelativePath)
		}
		if expectedNode.ModTimeUnixNano != 0 && actualNode.ModTimeUnixNano != expectedNode.ModTimeUnixNano {
			return fmt.Errorf("subtree path %q changed modification time", expectedNode.RelativePath)
		}
	}
	return nil
}

func normalizeSubtreeNode(node SubtreeNode) SubtreeNode {
	node.RelativePath = strings.TrimSpace(strings.ReplaceAll(node.RelativePath, "\\", "/"))
	node.Kind = strings.TrimSpace(strings.ToLower(node.Kind))
	node.ObjectID = strings.TrimSpace(node.ObjectID)
	node.SHA1 = strings.ToUpper(strings.TrimSpace(node.SHA1))
	return node
}

func sortSubtreeNodes(nodes []SubtreeNode) {
	sort.Slice(nodes, func(i, j int) bool {
		left := syncplanpkg.PathKey(nodes[i].RelativePath)
		right := syncplanpkg.PathKey(nodes[j].RelativePath)
		if left != right {
			return left < right
		}
		return nodes[i].Kind < nodes[j].Kind
	})
}
