package syncexec

import (
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

// Graph is the deterministic dependency graph used by sync execution.
type Graph struct {
	Dependencies [][]int
	Dependents   [][]int
}

func ResolveJobs(jobs int) (int, error) {
	if jobs <= 0 {
		return 0, fmt.Errorf("--jobs must be > 0")
	}
	return jobs, nil
}

func destructiveDirectorySide(item syncplanpkg.Item) string {
	switch item.Action {
	case "delete-remote":
		if item.Kind == "directory" {
			return "remote"
		}
	case "replace-remote":
		if item.ReplacesKind == "directory" {
			return "remote"
		}
	case "delete-local":
		if item.Kind == "directory" {
			return "local"
		}
	case "replace-local":
		if item.ReplacesKind == "directory" {
			return "local"
		}
	}
	return ""
}

func validateDestructiveSubtreeCoverage(plan syncplanpkg.Plan) error {
	for _, root := range plan.Items {
		side := destructiveDirectorySide(root)
		if side == "" {
			continue
		}
		rootKey := syncplanpkg.PathKey(root.RelativePath)
		if rootKey == "" {
			return fmt.Errorf("destructive %s subtree root has an empty path", side)
		}
		expectedReason := fmt.Sprintf("covered-by-%s:%s", root.Action, root.RelativePath)
		prefix := strings.TrimRight(rootKey, "/") + "/"
		for _, item := range plan.Items {
			key := syncplanpkg.PathKey(item.RelativePath)
			if key == "" || key == rootKey || !strings.HasPrefix(key, prefix) {
				continue
			}
			if item.Action != "skip" || strings.TrimSpace(item.Reason) != expectedReason {
				return fmt.Errorf("destructive %s subtree %q overlaps descendant action %q at %q; descendants must be reviewed covered skips", side, root.RelativePath, item.Action, item.RelativePath)
			}
		}
	}
	return nil
}

func BuildGraph(plan syncplanpkg.Plan) (Graph, error) {
	graph := Graph{
		Dependencies: make([][]int, len(plan.Items)),
		Dependents:   make([][]int, len(plan.Items)),
	}
	if err := validateDestructiveSubtreeCoverage(plan); err != nil {
		return graph, err
	}
	remoteDirectoryBarriers := make(map[string]int)
	localDirectoryBarriers := make(map[string]int)
	replacementBarriers := make(map[string]int)
	for index, item := range plan.Items {
		key := syncplanpkg.PathKey(item.RelativePath)
		if ItemCreatesRemoteDirectory(item) {
			if previous, exists := remoteDirectoryBarriers[key]; exists && previous != index {
				return graph, fmt.Errorf("duplicate remote sync directory barrier for %q", item.RelativePath)
			}
			remoteDirectoryBarriers[key] = index
		}
		if ItemCreatesLocalDirectory(item) {
			if previous, exists := localDirectoryBarriers[key]; exists && previous != index {
				return graph, fmt.Errorf("duplicate local sync directory barrier for %q", item.RelativePath)
			}
			localDirectoryBarriers[key] = index
		}
		if item.Action == "replace-remote" || item.Action == "replace-local" || item.Action == "delete-remote" || item.Action == "delete-local" {
			replacementBarriers[key] = index
		}
	}

	lastRemoteDestructive := make(map[string]int)
	lastLocalDestructive := make(map[string]int)
	for index, item := range plan.Items {
		dependencies := make(map[int]struct{})
		key := syncplanpkg.PathKey(item.RelativePath)
		if ItemWritesRemote(item) {
			if dependency, ok := nearestDirectoryBarrier(key, remoteDirectoryBarriers); ok && dependency != index {
				dependencies[dependency] = struct{}{}
			}
		}
		if ItemWritesLocal(item) {
			if dependency, ok := nearestDirectoryBarrier(key, localDirectoryBarriers); ok && dependency != index {
				dependencies[dependency] = struct{}{}
			}
		}
		if item.Action == "skip" {
			if dependency, ok := nearestDirectoryBarrier(key, replacementBarriers); ok && dependency != index {
				dependencies[dependency] = struct{}{}
			}
		}
		switch item.Action {
		case "replace-remote", "delete-remote":
			parent := parentPathKey(key)
			if previous, ok := lastRemoteDestructive[parent]; ok && previous != index {
				dependencies[previous] = struct{}{}
			}
			lastRemoteDestructive[parent] = index
		case "replace-local", "delete-local":
			parent := parentPathKey(key)
			if previous, ok := lastLocalDestructive[parent]; ok && previous != index {
				dependencies[previous] = struct{}{}
			}
			lastLocalDestructive[parent] = index
		}
		graph.Dependencies[index] = sortedDependencySet(dependencies)
		for _, dependency := range graph.Dependencies[index] {
			if dependency < 0 || dependency >= len(plan.Items) {
				return graph, fmt.Errorf("sync dependency index %d for %q is out of range", dependency, item.RelativePath)
			}
			graph.Dependents[dependency] = append(graph.Dependents[dependency], index)
		}
	}
	for index := range graph.Dependents {
		sort.Ints(graph.Dependents[index])
	}
	if err := ValidateGraph(graph); err != nil {
		return graph, err
	}
	return graph, nil
}

func nearestDirectoryBarrier(key string, barriers map[string]int) (int, bool) {
	for parent := parentPathKey(key); parent != ""; parent = parentPathKey(parent) {
		if index, ok := barriers[parent]; ok {
			return index, true
		}
	}
	return 0, false
}

func parentPathKey(key string) string {
	if key == "" {
		return ""
	}
	parent := pathpkg.Dir(key)
	if parent == "." || parent == "/" || parent == key {
		return ""
	}
	return parent
}

func sortedDependencySet(dependencies map[int]struct{}) []int {
	if len(dependencies) == 0 {
		return nil
	}
	result := make([]int, 0, len(dependencies))
	for index := range dependencies {
		result = append(result, index)
	}
	sort.Ints(result)
	return result
}

func ValidateGraph(graph Graph) error {
	if len(graph.Dependencies) != len(graph.Dependents) {
		return fmt.Errorf("invalid sync execution graph dimensions")
	}
	indegree := make([]int, len(graph.Dependencies))
	ready := make([]int, 0, len(indegree))
	for index, dependencies := range graph.Dependencies {
		indegree[index] = len(dependencies)
		if indegree[index] == 0 {
			ready = append(ready, index)
		}
	}
	processed := 0
	for len(ready) > 0 {
		index := ready[0]
		ready = ready[1:]
		processed++
		for _, dependent := range graph.Dependents[index] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if processed != len(graph.Dependencies) {
		return fmt.Errorf("sync execution dependency graph contains a cycle")
	}
	return nil
}

func ItemWritesRemote(item syncplanpkg.Item) bool {
	return item.Action == "upload" || item.Action == "replace-remote" || item.Action == "delete-remote"
}

func ItemWritesLocal(item syncplanpkg.Item) bool {
	return item.Action == "download" || item.Action == "replace-local" || item.Action == "delete-local"
}

func ItemCreatesRemoteDirectory(item syncplanpkg.Item) bool {
	return item.Kind == "directory" && (item.Action == "upload" || item.Action == "replace-remote")
}

func ItemCreatesLocalDirectory(item syncplanpkg.Item) bool {
	return item.Kind == "directory" && (item.Action == "download" || item.Action == "replace-local")
}

func PlanFileTransferCount(plan syncplanpkg.Plan) int {
	count := 0
	for _, item := range plan.Items {
		if ItemUsesFileTransfer(item) {
			count++
		}
	}
	return count
}

func TransferBudget(workersPerInterface, jobs, fileTransfers int) (workersPerTransfer, parallelTransfers int, err error) {
	if workersPerInterface <= 0 {
		return 0, 0, fmt.Errorf("workers-per-interface must be > 0")
	}
	if jobs <= 0 {
		return 0, 0, fmt.Errorf("--jobs must be > 0")
	}
	if fileTransfers <= 0 {
		return workersPerInterface, 0, nil
	}
	parallelTransfers = jobs
	if parallelTransfers > fileTransfers {
		parallelTransfers = fileTransfers
	}
	if parallelTransfers > workersPerInterface {
		parallelTransfers = workersPerInterface
	}
	if parallelTransfers < 1 {
		parallelTransfers = 1
	}
	return workersPerInterface / parallelTransfers, parallelTransfers, nil
}

func TransferWorkerLimit(workersPerInterface, jobs, fileTransfers int) (int, error) {
	workers, _, err := TransferBudget(workersPerInterface, jobs, fileTransfers)
	return workers, err
}

func ItemUsesFileTransfer(item syncplanpkg.Item) bool {
	if item.Kind == "directory" {
		return false
	}
	switch item.Action {
	case "upload", "download", "replace-remote", "replace-local":
		return true
	default:
		return false
	}
}

func PlanFileTransferNeeds(plan syncplanpkg.Plan) (upload, download bool) {
	for _, item := range plan.Items {
		if item.Kind == "directory" {
			continue
		}
		switch item.Action {
		case "upload", "replace-remote":
			upload = true
		case "download", "replace-local":
			download = true
		}
	}
	return upload, download
}

func PlanHasWrites(plan syncplanpkg.Plan) bool {
	for _, item := range plan.Items {
		if item.Action != "skip" {
			return true
		}
	}
	return false
}
