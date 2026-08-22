package syncjournal

import (
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

// PostconditionEqual compares persisted completion evidence with a freshly
// captured postcondition. Empty optional identity/digest/time fields in the
// expected value intentionally mean "not bound" for backward compatibility.
func PostconditionEqual(expected, actual *Postcondition) bool {
	if expected == nil || actual == nil || expected.Side != actual.Side || expected.Exists != actual.Exists {
		return false
	}
	if !expected.Exists {
		return true
	}
	if expected.Kind != actual.Kind || expected.Size != actual.Size {
		return false
	}
	if expected.Side == "remote" && expected.RemoteID != "" && expected.RemoteID != actual.RemoteID {
		return false
	}
	if expected.SHA1 != "" && !strings.EqualFold(expected.SHA1, actual.SHA1) {
		return false
	}
	if expected.ModTimeUnixNano != 0 && expected.ModTimeUnixNano != actual.ModTimeUnixNano {
		return false
	}
	return true
}

func plannedLocalKind(item syncplanpkg.Item) string {
	if item.Action == "replace-local" {
		return item.ReplacesKind
	}
	return item.Kind
}

func plannedRemoteKind(item syncplanpkg.Item) string {
	if item.Action == "replace-remote" {
		return item.ReplacesKind
	}
	return item.Kind
}

func sortedEntryKeys(entries map[string]syncplanpkg.Entry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedItemKeys(items map[string]syncplanpkg.Item) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RemoveInterruptedDownloadArtifacts removes resumable download implementation
// files from a freshly scanned local tree only when the journal proves that the
// corresponding file action was interrupted after its write phase began.
func RemoveInterruptedDownloadArtifacts(current map[string]syncplanpkg.Entry, journal Journal) {
	for index, stored := range journal.Items {
		if index < 0 || index >= len(journal.Plan.Items) {
			continue
		}
		item := journal.Plan.Items[index]
		if item.Kind == "directory" {
			continue
		}
		interruptedDownload := item.Action == "download" && stored.Phase == PhaseMutationStarted
		interruptedReplacement := item.Action == "replace-local" && (stored.Phase == PhaseWinnerStarted || stored.Phase == PhaseLoserRemoved || stored.Phase == PhaseMutationStarted)
		if !interruptedDownload && !interruptedReplacement {
			continue
		}
		relative := strings.ReplaceAll(item.RelativePath, "\\", "/")
		dir := pathpkg.Dir(relative)
		if dir == "." {
			dir = ""
		}
		base := pathpkg.Base(relative)
		for _, artifact := range []string{"." + base + ".115driver.part", "." + base + ".115driver.resume.json"} {
			candidate := artifact
			if dir != "" {
				candidate = dir + "/" + artifact
			}
			delete(current, syncplanpkg.PathKey(candidate))
		}
	}
}

// CompareExpectedLocalTree verifies that current local entries exactly match
// the local side projected by ExpectedPlan. It deliberately checks metadata
// available from a bounded tree scan only; completed-item content digests must
// still be verified separately through PostconditionEqual.
func CompareExpectedLocalTree(plan syncplanpkg.Plan, current map[string]syncplanpkg.Entry) error {
	expected := make(map[string]syncplanpkg.Item)
	for _, item := range plan.Items {
		if item.LocalPresent {
			expected[syncplanpkg.PathKey(item.RelativePath)] = item
		}
	}
	for _, key := range sortedEntryKeys(current) {
		entry := current[key]
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("local sync tree changed after planning: unexpected entry %q", entry.RelativePath)
		}
	}
	for _, key := range sortedItemKeys(expected) {
		item := expected[key]
		entry, ok := current[key]
		if !ok {
			return fmt.Errorf("local sync tree changed after planning: planned entry %q disappeared", item.RelativePath)
		}
		expectedKind := plannedLocalKind(item)
		if entry.Kind != expectedKind || entry.RelativePath != item.RelativePath {
			return fmt.Errorf("local sync entry %q changed identity or type after planning", item.RelativePath)
		}
		if expectedKind == "file" && entry.Size != item.LocalSize {
			return fmt.Errorf("local sync file %q changed size after planning: expected %d got %d", item.RelativePath, item.LocalSize, entry.Size)
		}
		if item.LocalModTimeUnixNano != 0 && entry.ModTimeUnixNano != item.LocalModTimeUnixNano {
			return fmt.Errorf("local sync entry %q changed modification time after planning", item.RelativePath)
		}
	}
	return nil
}

// CompareExpectedRemoteTree verifies the remote side projected by ExpectedPlan.
// resolveSHA1 is called only when an expected file digest is bound but the tree
// scan did not provide one.
func CompareExpectedRemoteTree(plan syncplanpkg.Plan, current map[string]syncplanpkg.Entry, resolveSHA1 func(syncplanpkg.Entry) (string, error)) error {
	expected := make(map[string]syncplanpkg.Item)
	for _, item := range plan.Items {
		if item.RemotePresent {
			expected[syncplanpkg.PathKey(item.RelativePath)] = item
		}
	}
	for _, key := range sortedEntryKeys(current) {
		entry := current[key]
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("remote sync tree changed after planning: unexpected entry %q", entry.RelativePath)
		}
	}
	for _, key := range sortedItemKeys(expected) {
		item := expected[key]
		entry, ok := current[key]
		if !ok {
			return fmt.Errorf("remote sync tree changed after planning: planned entry %q disappeared", item.RelativePath)
		}
		expectedKind := plannedRemoteKind(item)
		if entry.Kind != expectedKind || entry.RelativePath != item.RelativePath || strings.TrimSpace(entry.RemoteID) == "" || entry.RemoteID != item.RemoteID {
			return fmt.Errorf("remote sync entry %q changed identity or type after planning", item.RelativePath)
		}
		if expectedKind == "file" {
			if entry.Size != item.RemoteSize {
				return fmt.Errorf("remote sync file %q changed size after planning: expected %d got %d", item.RelativePath, item.RemoteSize, entry.Size)
			}
			if item.RemoteSHA1 != "" {
				currentSHA1 := strings.ToUpper(strings.TrimSpace(entry.SHA1))
				if currentSHA1 == "" {
					if resolveSHA1 == nil {
						return fmt.Errorf("remote SHA1 resolver is required for %q", item.RelativePath)
					}
					resolved, err := resolveSHA1(entry)
					if err != nil {
						return err
					}
					currentSHA1 = strings.ToUpper(strings.TrimSpace(resolved))
				}
				if !strings.EqualFold(currentSHA1, item.RemoteSHA1) {
					return fmt.Errorf("remote sync file %q changed SHA1 after planning", item.RelativePath)
				}
			}
		}
		if item.RemoteModTimeUnixNano != 0 && entry.ModTimeUnixNano != item.RemoteModTimeUnixNano {
			return fmt.Errorf("remote sync entry %q changed modification time after planning", item.RelativePath)
		}
	}
	return nil
}
