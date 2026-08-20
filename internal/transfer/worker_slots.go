package transfer

func normalizeWorkersPerInterface(value int) int {
	if value == 0 {
		return 1
	}
	return value
}

func expandWorkerPaths(paths []NetworkPath, workersPerInterface int) []NetworkPath {
	workersPerInterface = normalizeWorkersPerInterface(workersPerInterface)
	if workersPerInterface <= 0 || len(paths) == 0 {
		return nil
	}
	workers := make([]NetworkPath, 0, len(paths)*workersPerInterface)
	for _, path := range paths {
		for slot := 0; slot < workersPerInterface; slot++ {
			workers = append(workers, cloneNetworkPath(path))
		}
	}
	return workers
}

func availablePhysicalInterfaceCount(paths []NetworkPath, health *NetworkHealthTracker) int {
	seen := make(map[int]struct{}, len(paths))
	for _, path := range paths {
		if _, exists := seen[path.InterfaceIndex]; exists {
			continue
		}
		if !health.Available(path) {
			continue
		}
		seen[path.InterfaceIndex] = struct{}{}
	}
	return len(seen)
}
