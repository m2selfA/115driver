package syncplan

import (
	"crypto/sha256"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/SheltonZhu/115driver/internal/remotetree"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

const (
	ModeConservative  = "conservative-policy"
	DirectionBoth     = "both"
	DirectionUpload   = "upload"
	DirectionDownload = "download"
	ConflictError     = "error"
	ConflictLocal     = "prefer-local"
	ConflictRemote    = "prefer-remote"
)

type Options struct {
	Direction        string
	ConflictPolicy   string
	DeleteExtraneous bool
}

func ResolveOptions(direction, conflictPolicy string) (Options, error) {
	return ResolveOptionsWithDelete(direction, conflictPolicy, false)
}

func ResolveOptionsWithDelete(direction, conflictPolicy string, deleteExtraneous bool) (Options, error) {
	options := Options{
		Direction:        strings.ToLower(strings.TrimSpace(direction)),
		ConflictPolicy:   strings.ToLower(strings.TrimSpace(conflictPolicy)),
		DeleteExtraneous: deleteExtraneous,
	}
	if options.Direction == "" {
		options.Direction = DirectionBoth
	}
	if options.ConflictPolicy == "" {
		options.ConflictPolicy = ConflictError
	}
	switch options.Direction {
	case DirectionBoth, DirectionUpload, DirectionDownload:
	default:
		return Options{}, fmt.Errorf("invalid --direction %q: expected both, upload, or download", direction)
	}
	switch options.ConflictPolicy {
	case ConflictError, ConflictLocal, ConflictRemote:
	default:
		return Options{}, fmt.Errorf("invalid --conflict %q: expected error, prefer-local, or prefer-remote", conflictPolicy)
	}
	if options.Direction == DirectionUpload && options.ConflictPolicy == ConflictRemote {
		return Options{}, fmt.Errorf("--conflict prefer-remote requires --direction both or download")
	}
	if options.Direction == DirectionDownload && options.ConflictPolicy == ConflictLocal {
		return Options{}, fmt.Errorf("--conflict prefer-local requires --direction both or upload")
	}
	if options.DeleteExtraneous && options.Direction == DirectionBoth {
		return Options{}, fmt.Errorf("--delete requires explicit --direction upload or download")
	}
	return options, nil
}

type Entry struct {
	RelativePath    string
	Kind            string
	LocalPath       string
	RemotePath      string
	RemoteID        string
	Size            int64
	SHA1            string
	ModTimeUnixNano int64
}

type Item struct {
	RelativePath  string `json:"relative_path"`
	Action        string `json:"action"`
	Kind          string `json:"kind"`
	Reason        string `json:"reason"`
	LocalPresent  bool   `json:"local_present"`
	RemotePresent bool   `json:"remote_present"`
	LocalPath     string `json:"local_path"`
	RemotePath    string `json:"remote_path"`
	RemoteID      string `json:"remote_id,omitempty"`
	LocalSize     int64  `json:"local_size,omitempty"`
	RemoteSize    int64  `json:"remote_size,omitempty"`
	LocalSHA1     string `json:"local_sha1,omitempty"`
	RemoteSHA1    string `json:"remote_sha1,omitempty"`
	Destructive   bool   `json:"destructive,omitempty"`
	ReplacesKind  string `json:"replaces_kind,omitempty"`

	LocalModTimeUnixNano  int64                     `json:"-"`
	RemoteModTimeUnixNano int64                     `json:"-"`
	LocalPreparedDigest   *uploadpkg.PreparedDigest `json:"-"`
}

type Plan struct {
	Operation                string `json:"operation"`
	PlanID                   string `json:"plan_id"`
	DryRun                   bool   `json:"dry_run"`
	Mode                     string `json:"mode"`
	Direction                string `json:"direction"`
	ConflictPolicy           string `json:"conflict_policy"`
	DeleteExtraneous         bool   `json:"delete"`
	Ready                    bool   `json:"ready"`
	ChangeActions            int    `json:"change_actions"`
	LocalRoot                string `json:"local_root"`
	RemoteRoot               string `json:"remote_root"`
	RemoteRootID             string `json:"remote_root_id"`
	LocalFiles               int    `json:"local_files"`
	LocalDirs                int    `json:"local_directories"`
	RemoteFiles              int    `json:"remote_files"`
	RemoteDirs               int    `json:"remote_directories"`
	UploadFiles              int    `json:"upload_files"`
	UploadDirs               int    `json:"upload_directories"`
	UploadBytes              int64  `json:"upload_bytes"`
	DownloadFiles            int    `json:"download_files"`
	DownloadDirs             int    `json:"download_directories"`
	DownloadBytes            int64  `json:"download_bytes"`
	DeleteRemoteRoots        int    `json:"delete_remote_roots"`
	DeleteRemoteFiles        int    `json:"delete_remote_files"`
	DeleteRemoteDirs         int    `json:"delete_remote_directories"`
	DeleteRemoteBytes        int64  `json:"delete_remote_bytes"`
	DeleteLocalRoots         int    `json:"delete_local_roots"`
	DeleteLocalFiles         int    `json:"delete_local_files"`
	DeleteLocalDirs          int    `json:"delete_local_directories"`
	DeleteLocalBytes         int64  `json:"delete_local_bytes"`
	CoveredByDelete          int    `json:"covered_by_delete"`
	SkippedFiles             int    `json:"skipped_files"`
	SkippedDirs              int    `json:"skipped_directories"`
	Conflicts                int    `json:"conflicts"`
	ResolvedConflicts        int    `json:"resolved_conflicts"`
	DestructiveActions       int    `json:"destructive_actions"`
	RequiresAllowDestructive bool   `json:"requires_allow_destructive"`
	ChecksummedFiles         int    `json:"checksummed_files"`
	ChecksummedBytes         int64  `json:"checksummed_bytes"`
	Items                    []Item `json:"items"`
}

// DeleteTotals returns the collapsed mirror-delete roots plus the complete
// affected subtree item/byte totals. Execution-only safety gates in the CLI and
// MCP server share this accounting so their limits cannot drift.
func (plan Plan) DeleteTotals() (roots, items int, bytes int64) {
	roots = plan.DeleteRemoteRoots + plan.DeleteLocalRoots
	items = plan.DeleteRemoteFiles + plan.DeleteRemoteDirs + plan.DeleteLocalFiles + plan.DeleteLocalDirs
	bytes = plan.DeleteRemoteBytes + plan.DeleteLocalBytes
	return roots, items, bytes
}

type Resolvers struct {
	RemoteSHA1  func(Entry) (string, error)
	LocalDigest func(Entry) (*uploadpkg.PreparedDigest, error)
}

type replacementPrefix struct {
	Key        string
	Action     string
	WinnerKind string
}

func Build(localEntries, remoteEntries map[string]Entry, localRoot, remoteRoot, remoteRootID string, requested Options, resolvers Resolvers) (Plan, error) {
	options, err := ResolveOptionsWithDelete(requested.Direction, requested.ConflictPolicy, requested.DeleteExtraneous)
	plan := Plan{
		Operation: "sync", DryRun: true, Mode: ModeConservative, Direction: options.Direction,
		ConflictPolicy: options.ConflictPolicy, DeleteExtraneous: options.DeleteExtraneous,
		LocalRoot: localRoot, RemoteRoot: remoteRoot, RemoteRootID: remoteRootID,
	}
	if err != nil {
		return plan, err
	}
	if strings.TrimSpace(localRoot) == "" {
		return plan, fmt.Errorf("sync local root is empty")
	}
	if strings.TrimSpace(remoteRoot) == "" || strings.TrimSpace(remoteRootID) == "" {
		return plan, fmt.Errorf("sync remote root identity is incomplete")
	}
	for key, entry := range localEntries {
		if PathKey(entry.RelativePath) != key {
			return plan, fmt.Errorf("local sync entry %q has inconsistent path key %q", entry.RelativePath, key)
		}
		if entry.Kind == "directory" {
			plan.LocalDirs++
		} else if entry.Kind == "file" {
			plan.LocalFiles++
		} else {
			return plan, fmt.Errorf("local sync entry %q has unsupported kind %q", entry.RelativePath, entry.Kind)
		}
	}
	for key, entry := range remoteEntries {
		if PathKey(entry.RelativePath) != key {
			return plan, fmt.Errorf("remote sync entry %q has inconsistent path key %q", entry.RelativePath, key)
		}
		if entry.Kind == "directory" {
			plan.RemoteDirs++
		} else if entry.Kind == "file" {
			plan.RemoteFiles++
		} else {
			return plan, fmt.Errorf("remote sync entry %q has unsupported kind %q", entry.RelativePath, entry.Kind)
		}
	}

	keys := make([]string, 0, len(localEntries)+len(remoteEntries))
	seen := make(map[string]struct{}, len(localEntries)+len(remoteEntries))
	for key := range localEntries {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range remoteEntries {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		leftDepth := strings.Count(keys[i], "/")
		rightDepth := strings.Count(keys[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return keys[i] < keys[j]
	})

	ensureLocalContentSnapshot := func(local Entry, item *Item) error {
		if item == nil || local.Kind != "file" {
			return nil
		}
		if item.LocalPreparedDigest != nil && strings.TrimSpace(item.LocalSHA1) != "" {
			return nil
		}
		if resolvers.LocalDigest == nil {
			return fmt.Errorf("local digest resolver is required for %q", local.RelativePath)
		}
		digest, hashErr := resolvers.LocalDigest(local)
		if hashErr != nil {
			return hashErr
		}
		if digest == nil {
			return fmt.Errorf("local digest resolver returned nil for %q", local.RelativePath)
		}
		plan.ChecksummedFiles++
		plan.ChecksummedBytes += local.Size
		item.LocalSHA1 = strings.ToUpper(strings.TrimSpace(digest.SHA1))
		item.LocalPreparedDigest = digest
		return nil
	}

	ensureRemoteContentSnapshot := func(remote Entry, item *Item) error {
		if item == nil || remote.Kind != "file" {
			return nil
		}
		if strings.TrimSpace(item.RemoteSHA1) != "" {
			item.RemoteSHA1 = strings.ToUpper(strings.TrimSpace(item.RemoteSHA1))
			return nil
		}
		if resolvers.RemoteSHA1 == nil {
			return fmt.Errorf("remote SHA1 resolver is required for %q", remote.RelativePath)
		}
		sha1, resolveErr := resolvers.RemoteSHA1(remote)
		if resolveErr != nil {
			return resolveErr
		}
		sha1 = strings.ToUpper(strings.TrimSpace(sha1))
		if sha1 == "" {
			return fmt.Errorf("remote SHA1 is unavailable for %q", remote.RelativePath)
		}
		item.RemoteSHA1 = sha1
		return nil
	}

	conflictPrefixes := make([]string, 0)
	replacementPrefixes := make([]replacementPrefix, 0)
	deletePrefixes := make([]replacementPrefix, 0)
	plan.Items = make([]Item, 0, len(keys))
	for _, key := range keys {
		local, localOK := localEntries[key]
		remote, remoteOK := remoteEntries[key]
		item := newItem(localRoot, remoteRoot, local, localOK, remote, remoteOK)
		if ancestor := conflictAncestor(key, conflictPrefixes); ancestor != "" {
			item.Action = "conflict"
			item.Reason = "blocked-by-type-conflict:" + ancestor
			plan.Conflicts++
			plan.Items = append(plan.Items, item)
			continue
		}
		if replacement := replacementAncestor(key, replacementPrefixes); replacement != nil && replacement.WinnerKind == "file" {
			item.Action = "skip"
			item.Reason = "covered-by-" + replacement.Action + ":" + replacement.Key
			if replacement.Action == "replace-local" && localOK && local.Kind == "file" {
				if err := ensureLocalContentSnapshot(local, &item); err != nil {
					return plan, err
				}
			}
			if replacement.Action == "replace-remote" && remoteOK && remote.Kind == "file" {
				if err := ensureRemoteContentSnapshot(remote, &item); err != nil {
					return plan, err
				}
			}
			addCoveredDestructiveStats(&plan, replacement.Action, local, localOK, remote, remoteOK)
			addSkipStats(&plan, item.Kind)
			plan.Items = append(plan.Items, item)
			continue
		}
		if deletion := replacementAncestor(key, deletePrefixes); deletion != nil {
			item.Action = "skip"
			item.Reason = "covered-by-" + deletion.Action + ":" + deletion.Key
			if deletion.Action == "delete-local" && localOK && local.Kind == "file" {
				if err := ensureLocalContentSnapshot(local, &item); err != nil {
					return plan, err
				}
			}
			if deletion.Action == "delete-remote" && remoteOK && remote.Kind == "file" {
				if err := ensureRemoteContentSnapshot(remote, &item); err != nil {
					return plan, err
				}
			}
			plan.CoveredByDelete++
			addCoveredDestructiveStats(&plan, deletion.Action, local, localOK, remote, remoteOK)
			addSkipStats(&plan, item.Kind)
			plan.Items = append(plan.Items, item)
			continue
		}
		switch {
		case localOK && !remoteOK:
			if directionAllows(options.Direction, DirectionUpload) {
				item.Action = "upload"
				item.Reason = "local-only"
				addUploadStats(&plan, local)
			} else if options.DeleteExtraneous && options.Direction == DirectionDownload {
				item.Action = "delete-local"
				item.Reason = "mirror-delete:local-only"
				item.Destructive = true
				plan.DestructiveActions++
				addDeleteStats(&plan, item.Action, local)
				if local.Kind == "directory" {
					deletePrefixes = append(deletePrefixes, replacementPrefix{Key: key, Action: item.Action, WinnerKind: local.Kind})
				}
			} else {
				item.Action = "skip"
				item.Reason = "direction-excludes-upload"
				addSkipStats(&plan, item.Kind)
			}
		case !localOK && remoteOK:
			if directionAllows(options.Direction, DirectionDownload) {
				item.Action = "download"
				item.Reason = "remote-only"
				addDownloadStats(&plan, remote)
			} else if options.DeleteExtraneous && options.Direction == DirectionUpload {
				item.Action = "delete-remote"
				item.Reason = "mirror-delete:remote-only"
				item.Destructive = true
				plan.DestructiveActions++
				addDeleteStats(&plan, item.Action, remote)
				if remote.Kind == "directory" {
					deletePrefixes = append(deletePrefixes, replacementPrefix{Key: key, Action: item.Action, WinnerKind: remote.Kind})
				}
			} else {
				item.Action = "skip"
				item.Reason = "direction-excludes-download"
				addSkipStats(&plan, item.Kind)
			}
		case local.Kind != remote.Kind:
			resolved := applyConflictPolicy(&plan, &item, "type-mismatch", local, remote, options)
			if resolved {
				replacementPrefixes = append(replacementPrefixes, replacementPrefix{Key: key, Action: item.Action, WinnerKind: item.Kind})
			} else {
				conflictPrefixes = append(conflictPrefixes, key)
			}
		case local.Kind == "directory":
			item.Action = "skip"
			item.Reason = "directory-present-both"
			plan.SkippedDirs++
		default:
			if local.Size != remote.Size {
				applyConflictPolicy(&plan, &item, "size-mismatch", local, remote, options)
				break
			}
			remoteSHA1 := strings.ToUpper(strings.TrimSpace(remote.SHA1))
			if remoteSHA1 == "" {
				if resolvers.RemoteSHA1 == nil {
					return plan, fmt.Errorf("remote SHA1 resolver is required for %q", remote.RelativePath)
				}
				remoteSHA1, err = resolvers.RemoteSHA1(remote)
				if err != nil {
					return plan, err
				}
				remoteSHA1 = strings.ToUpper(strings.TrimSpace(remoteSHA1))
			}
			item.RemoteSHA1 = remoteSHA1
			if remoteSHA1 == "" {
				item.Action = "conflict"
				item.Reason = "remote-sha1-unavailable"
				plan.Conflicts++
				break
			}
			if err := ensureLocalContentSnapshot(local, &item); err != nil {
				return plan, err
			}
			if strings.EqualFold(item.LocalSHA1, remoteSHA1) {
				item.Action = "skip"
				item.Reason = "sha1-match"
				plan.SkippedFiles++
			} else {
				applyConflictPolicy(&plan, &item, "sha1-mismatch", local, remote, options)
			}
		}
		if localOK && local.Kind == "file" {
			switch item.Action {
			case "upload", "replace-remote", "replace-local", "delete-local":
				if err := ensureLocalContentSnapshot(local, &item); err != nil {
					return plan, err
				}
			}
		}
		if remoteOK && remote.Kind == "file" {
			switch item.Action {
			case "download", "replace-remote", "replace-local", "delete-remote":
				if err := ensureRemoteContentSnapshot(remote, &item); err != nil {
					return plan, err
				}
			}
		}
		plan.Items = append(plan.Items, item)
	}
	plan.Ready = plan.Conflicts == 0
	plan.ChangeActions = ChangeCount(plan)
	plan.RequiresAllowDestructive = plan.DestructiveActions > 0
	plan.PlanID = Fingerprint(plan)
	return plan, nil
}

func Fingerprint(plan Plan) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write([]byte(value))
	}
	write("115driver-sync-plan-v1")
	for _, value := range []string{
		plan.Mode,
		plan.Direction,
		plan.ConflictPolicy,
		fmt.Sprintf("%t", plan.DeleteExtraneous),
		plan.LocalRoot,
		plan.RemoteRoot,
		plan.RemoteRootID,
	} {
		write(value)
	}
	for _, item := range plan.Items {
		for _, value := range []string{
			item.RelativePath,
			item.Action,
			item.Kind,
			item.Reason,
			fmt.Sprintf("%t", item.LocalPresent),
			fmt.Sprintf("%t", item.RemotePresent),
			item.LocalPath,
			item.RemotePath,
			item.RemoteID,
			fmt.Sprintf("%d", item.LocalSize),
			fmt.Sprintf("%d", item.RemoteSize),
			strings.ToUpper(strings.TrimSpace(item.LocalSHA1)),
			strings.ToUpper(strings.TrimSpace(item.RemoteSHA1)),
			fmt.Sprintf("%t", item.Destructive),
			item.ReplacesKind,
			fmt.Sprintf("%d", item.LocalModTimeUnixNano),
			fmt.Sprintf("%d", item.RemoteModTimeUnixNano),
		} {
			write(value)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func ChangeCount(plan Plan) int {
	count := 0
	for _, item := range plan.Items {
		switch item.Action {
		case "upload", "download", "replace-remote", "replace-local", "delete-remote", "delete-local":
			count++
		}
	}
	return count
}

func ValidateCheck(plan Plan) error {
	if plan.Conflicts > 0 || !plan.Ready {
		return fmt.Errorf("sync check found %d unresolved conflict(s)", plan.Conflicts)
	}
	if plan.ChangeActions > 0 {
		return fmt.Errorf("sync check found %d planned change action(s)", plan.ChangeActions)
	}
	return nil
}

func AddEntry(entries map[string]Entry, entry Entry, side string) error {
	key := PathKey(entry.RelativePath)
	if key == "" {
		return fmt.Errorf("%s sync entry has an empty relative path", side)
	}
	if previous, exists := entries[key]; exists {
		return fmt.Errorf("%s sync tree has multiple entries mapping to the same local path: %q and %q", side, previous.RelativePath, entry.RelativePath)
	}
	entries[key] = entry
	return nil
}

func ValidateRelativePath(relative string) error {
	if relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "/") {
		return fmt.Errorf("invalid relative path")
	}
	if strings.Contains(relative, "\\") {
		return fmt.Errorf("path contains a backslash that cannot be mapped portably")
	}
	for _, component := range strings.Split(relative, "/") {
		if err := remotetree.ValidateEntryName(component); err != nil {
			return err
		}
	}
	return nil
}

func PathKey(relative string) string {
	cleaned := pathpkg.Clean(strings.ReplaceAll(relative, "\\", "/"))
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	value := filepath.Clean(filepath.FromSlash(cleaned))
	if runtime.GOOS == "windows" {
		value = strings.ToLower(value)
	}
	return filepath.ToSlash(value)
}

func CanonicalRemoteRoot(remoteRoot string) string {
	cleaned := pathpkg.Clean(remoteRoot)
	if cleaned == "." || cleaned == "" {
		return "/"
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func RemoteChildPath(remoteRoot, relative string) string {
	joined := pathpkg.Join(remoteRoot, strings.ReplaceAll(relative, "\\", "/"))
	if strings.HasPrefix(remoteRoot, "/") && !strings.HasPrefix(joined, "/") {
		return "/" + joined
	}
	return joined
}

func newItem(localRoot, remoteRoot string, local Entry, localOK bool, remote Entry, remoteOK bool) Item {
	relative := local.RelativePath
	if relative == "" {
		relative = remote.RelativePath
	}
	kind := local.Kind
	if kind == "" {
		kind = remote.Kind
	}
	item := Item{
		RelativePath: relative, Kind: kind, LocalPresent: localOK, RemotePresent: remoteOK,
		LocalPath: filepath.Join(localRoot, filepath.FromSlash(relative)), RemotePath: RemoteChildPath(remoteRoot, relative),
	}
	if localOK {
		item.LocalPath = local.LocalPath
		item.LocalSize = local.Size
		item.LocalModTimeUnixNano = local.ModTimeUnixNano
	}
	if remoteOK {
		item.RemotePath = remote.RemotePath
		item.RemoteID = remote.RemoteID
		item.RemoteSize = remote.Size
		item.RemoteSHA1 = strings.ToUpper(strings.TrimSpace(remote.SHA1))
		item.RemoteModTimeUnixNano = remote.ModTimeUnixNano
	}
	return item
}

func directionAllows(direction, wanted string) bool {
	return direction == DirectionBoth || direction == wanted
}

func applyConflictPolicy(plan *Plan, item *Item, cause string, local, remote Entry, options Options) bool {
	switch options.ConflictPolicy {
	case ConflictLocal:
		item.Action = "replace-remote"
		item.Kind = local.Kind
		item.Reason = ConflictLocal + ":" + cause
		item.Destructive = true
		item.ReplacesKind = remote.Kind
		plan.ResolvedConflicts++
		plan.DestructiveActions++
		addDeleteStats(plan, item.Action, remote)
		addUploadStats(plan, local)
		return true
	case ConflictRemote:
		item.Action = "replace-local"
		item.Kind = remote.Kind
		item.Reason = ConflictRemote + ":" + cause
		item.Destructive = true
		item.ReplacesKind = local.Kind
		plan.ResolvedConflicts++
		plan.DestructiveActions++
		addDeleteStats(plan, item.Action, local)
		addDownloadStats(plan, remote)
		return true
	default:
		item.Action = "conflict"
		if local.Kind != remote.Kind {
			item.Kind = "mixed"
		}
		item.Reason = cause
		plan.Conflicts++
		return false
	}
}

func replacementAncestor(key string, prefixes []replacementPrefix) *replacementPrefix {
	for i := range prefixes {
		if strings.HasPrefix(key, prefixes[i].Key+"/") {
			return &prefixes[i]
		}
	}
	return nil
}

func conflictAncestor(key string, prefixes []string) string {
	for _, prefix := range prefixes {
		if strings.HasPrefix(key, prefix+"/") {
			return prefix
		}
	}
	return ""
}

func addUploadStats(plan *Plan, entry Entry) {
	if entry.Kind == "directory" {
		plan.UploadDirs++
		return
	}
	plan.UploadFiles++
	plan.UploadBytes += entry.Size
}

func addDownloadStats(plan *Plan, entry Entry) {
	if entry.Kind == "directory" {
		plan.DownloadDirs++
		return
	}
	plan.DownloadFiles++
	plan.DownloadBytes += entry.Size
}

func addDeleteStats(plan *Plan, action string, entry Entry) {
	switch action {
	case "delete-remote", "replace-remote":
		plan.DeleteRemoteRoots++
		addDeleteImpact(plan, action, entry)
	case "delete-local", "replace-local":
		plan.DeleteLocalRoots++
		addDeleteImpact(plan, action, entry)
	}
}

func addDeleteImpact(plan *Plan, action string, entry Entry) {
	switch action {
	case "delete-remote", "replace-remote":
		if entry.Kind == "directory" {
			plan.DeleteRemoteDirs++
		} else {
			plan.DeleteRemoteFiles++
			plan.DeleteRemoteBytes += entry.Size
		}
	case "delete-local", "replace-local":
		if entry.Kind == "directory" {
			plan.DeleteLocalDirs++
		} else {
			plan.DeleteLocalFiles++
			plan.DeleteLocalBytes += entry.Size
		}
	}
}

func addCoveredDestructiveStats(plan *Plan, action string, local Entry, localOK bool, remote Entry, remoteOK bool) {
	if (action == "delete-remote" || action == "replace-remote") && remoteOK {
		addDeleteImpact(plan, action, remote)
	}
	if (action == "delete-local" || action == "replace-local") && localOK {
		addDeleteImpact(plan, action, local)
	}
}

func addSkipStats(plan *Plan, kind string) {
	if kind == "directory" {
		plan.SkippedDirs++
		return
	}
	plan.SkippedFiles++
}
