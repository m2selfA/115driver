package cmd

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type syncReadOnlyClient struct {
	dirIDs       map[string]string
	lists        map[string][]driver.File
	files        map[string]driver.File
	getFileCalls int
}

func (client *syncReadOnlyClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	id, ok := client.dirIDs[dir]
	if !ok {
		return nil, driver.ErrNotExist
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(id)}, nil
}

func (client *syncReadOnlyClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *syncReadOnlyClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := client.lists[dirID]
	if offset >= int64(len(entries)) {
		empty := []driver.File{}
		return &empty, nil
	}
	end := offset + limit
	if end > int64(len(entries)) {
		end = int64(len(entries))
	}
	page := append([]driver.File(nil), entries[offset:end]...)
	return &page, nil
}

func (client *syncReadOnlyClient) GetFile(fileID string) (*driver.File, error) {
	client.getFileCalls++
	file, ok := client.files[fileID]
	if !ok {
		return nil, errors.New("file missing")
	}
	copy := file
	return &copy, nil
}

func testSyncSHA1(data string) string {
	digest := sha1.Sum([]byte(data))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func writeSyncTestFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func syncActionsByPath(plan syncPlan) map[string]syncPlanItem {
	items := make(map[string]syncPlanItem, len(plan.Items))
	for _, item := range plan.Items {
		items[item.RelativePath] = item
	}
	return items
}

func TestBuildSyncPlanClassifiesConservativeActionsWithExactSHA1(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "same.bin", "same")
	writeSyncTestFile(t, localRoot, "local-only.bin", "up")
	writeSyncTestFile(t, localRoot, "conflict.bin", "aaaa")
	writeSyncTestFile(t, localRoot, "localdir/child.bin", "x")
	if err := os.MkdirAll(filepath.Join(localRoot, "bothdir"), 0755); err != nil {
		t.Fatal(err)
	}

	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {
				{FileID: "same", Name: "same.bin", Size: 4, Sha1: testSyncSHA1("same")},
				{FileID: "down", Name: "remote-only.bin", Size: 4, Sha1: testSyncSHA1("down")},
				{FileID: "conflict", Name: "conflict.bin", Size: 4, Sha1: testSyncSHA1("bbbb")},
				{FileID: "bothdir", Name: "bothdir", IsDirectory: true},
				{FileID: "remotedir", Name: "remotedir", IsDirectory: true},
			},
			"bothdir":   {},
			"remotedir": {{FileID: "remote-child", Name: "child.bin", Size: 3, Sha1: testSyncSHA1("xyz")}},
		},
		files: map[string]driver.File{},
	}

	plan, err := buildSyncPlan(client, localRoot, "/remote")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || plan.Conflicts != 1 {
		t.Fatalf("unexpected readiness/conflicts: %#v", plan)
	}
	if plan.LocalFiles != 4 || plan.LocalDirs != 2 || plan.RemoteFiles != 4 || plan.RemoteDirs != 2 {
		t.Fatalf("unexpected tree counts: %#v", plan)
	}
	if plan.UploadFiles != 2 || plan.UploadDirs != 1 || plan.UploadBytes != 3 {
		t.Fatalf("unexpected upload counts: %#v", plan)
	}
	if plan.DownloadFiles != 2 || plan.DownloadDirs != 1 || plan.DownloadBytes != 7 {
		t.Fatalf("unexpected download counts: %#v", plan)
	}
	if plan.SkippedFiles != 1 || plan.SkippedDirs != 1 || plan.ChecksummedFiles != 4 || plan.ChecksummedBytes != 11 {
		t.Fatalf("unexpected skip/checksum counts: %#v", plan)
	}
	actions := syncActionsByPath(plan)
	for path, want := range map[string]string{
		"same.bin": "skip", "local-only.bin": "upload", "conflict.bin": "conflict", "bothdir": "skip",
		"localdir": "upload", "localdir/child.bin": "upload", "remote-only.bin": "download",
		"remotedir": "download", "remotedir/child.bin": "download",
	} {
		if got := actions[path].Action; got != want {
			t.Fatalf("action for %s: got %q want %q; item=%#v", path, got, want, actions[path])
		}
	}
	if actions["same.bin"].Reason != "sha1-match" || actions["conflict.bin"].Reason != "sha1-mismatch" {
		t.Fatalf("unexpected checksum reasons: same=%#v conflict=%#v", actions["same.bin"], actions["conflict.bin"])
	}
	if client.getFileCalls != 0 {
		t.Fatalf("list-provided SHA1 unexpectedly triggered GetFile: %d", client.getFileCalls)
	}
}

func TestBuildSyncPlanTypeConflictBlocksDescendantActions(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "node/child.bin", "child")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote-node", Name: "node", Size: 2, Sha1: testSyncSHA1("xx")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlan(client, localRoot, "/remote")
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	if actions["node"].Action != "conflict" || actions["node"].Reason != "type-mismatch" {
		t.Fatalf("root type conflict missing: %#v", actions["node"])
	}
	child := actions["node/child.bin"]
	if child.Action != "conflict" || !strings.HasPrefix(child.Reason, "blocked-by-type-conflict:") {
		t.Fatalf("descendant was not blocked by type conflict: %#v", child)
	}
	if plan.UploadFiles != 0 || plan.UploadDirs != 0 || plan.DownloadFiles != 0 || plan.DownloadDirs != 0 || plan.Conflicts != 2 {
		t.Fatalf("type-conflict subtree leaked executable actions: %#v", plan)
	}
}

func TestBuildSyncPlanFallsBackToReadOnlyGetFileForMissingRemoteSHA1(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "fallback.bin", "abc")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "fallback", Name: "fallback.bin", Size: 3}},
		},
		files: map[string]driver.File{
			"fallback": {FileID: "fallback", Name: "fallback.bin", Size: 3, Sha1: testSyncSHA1("abc")},
		},
	}
	plan, err := buildSyncPlan(client, localRoot, "/remote")
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["fallback.bin"]
	if item.Action != "skip" || item.Reason != "sha1-match" || client.getFileCalls != 1 {
		t.Fatalf("missing SHA1 fallback failed: item=%#v getFileCalls=%d", item, client.getFileCalls)
	}
	if !plan.Ready || plan.ChecksummedFiles != 1 || plan.ChecksummedBytes != 3 {
		t.Fatalf("unexpected fallback plan summary: %#v", plan)
	}
}

func TestBuildSyncPlanTreatsMissingRemoteSHA1AsConflictWithoutGuessing(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "unknown.bin", "abc")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "unknown", Name: "unknown.bin", Size: 3}},
		},
		files: map[string]driver.File{
			"unknown": {FileID: "unknown", Name: "unknown.bin", Size: 3},
		},
	}
	plan, err := buildSyncPlan(client, localRoot, "/remote")
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["unknown.bin"]
	if item.Action != "conflict" || item.Reason != "remote-sha1-unavailable" {
		t.Fatalf("missing remote SHA1 was guessed instead of conflicted: %#v", item)
	}
	if plan.Ready || plan.Conflicts != 1 || plan.ChecksummedFiles != 0 || client.getFileCalls != 1 {
		t.Fatalf("unexpected unavailable-SHA1 plan: plan=%#v calls=%d", plan, client.getFileCalls)
	}
}

func TestBuildSyncPlanRejectsAmbiguousRemotePathMapping(t *testing.T) {
	localRoot := t.TempDir()
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {
				{FileID: "first", Name: "dup.bin", Size: 1, Sha1: testSyncSHA1("a")},
				{FileID: "second", Name: "dup.bin", Size: 1, Sha1: testSyncSHA1("b")},
			},
		},
		files: map[string]driver.File{},
	}
	_, err := buildSyncPlan(client, localRoot, "/remote")
	if err == nil || !strings.Contains(err.Error(), "multiple entries mapping to the same local path") {
		t.Fatalf("ambiguous remote mapping was not rejected: %v", err)
	}
}

func TestResolveSyncPlanOptionsValidatesPolicyMatrix(t *testing.T) {
	options, err := resolveSyncPlanOptions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if options.Direction != syncDirectionBoth || options.ConflictPolicy != syncConflictError {
		t.Fatalf("unexpected default sync options: %#v", options)
	}
	options, err = resolveSyncPlanOptions(" UPLOAD ", " PREFER-LOCAL ")
	if err != nil {
		t.Fatal(err)
	}
	if options.Direction != syncDirectionUpload || options.ConflictPolicy != syncConflictLocal {
		t.Fatalf("sync options were not normalized: %#v", options)
	}
	for _, test := range []struct {
		direction string
		conflict  string
		message   string
	}{
		{direction: "sideways", conflict: "error", message: "invalid --direction"},
		{direction: "both", conflict: "newest", message: "invalid --conflict"},
		{direction: "upload", conflict: "prefer-remote", message: "requires --direction both or download"},
		{direction: "download", conflict: "prefer-local", message: "requires --direction both or upload"},
	} {
		if _, err := resolveSyncPlanOptions(test.direction, test.conflict); err == nil || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("unexpected option validation for direction=%q conflict=%q: %v", test.direction, test.conflict, err)
		}
	}
}

func TestSyncPlanChangeCountAndCheckStatus(t *testing.T) {
	converged := syncPlan{Ready: true, Items: []syncPlanItem{{RelativePath: "same.bin", Action: "skip", Kind: "file"}}}
	converged.ChangeActions = syncPlanChangeCount(converged)
	if converged.ChangeActions != 0 || validateSyncCheck(converged) != nil {
		t.Fatalf("converged plan failed sync check: %#v", converged)
	}
	changed := syncPlan{Ready: true, Items: []syncPlanItem{
		{RelativePath: "upload.bin", Action: "upload", Kind: "file"},
		{RelativePath: "delete.bin", Action: "delete-remote", Kind: "file"},
		{RelativePath: "covered.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-remote:dir"},
	}}
	changed.ChangeActions = syncPlanChangeCount(changed)
	if changed.ChangeActions != 2 {
		t.Fatalf("sync change count: got %d want 2", changed.ChangeActions)
	}
	if err := validateSyncCheck(changed); err == nil || !strings.Contains(err.Error(), "2 planned change action(s)") {
		t.Fatalf("changed plan passed sync check: %v", err)
	}
	conflicted := syncPlan{Ready: false, Conflicts: 2, Items: []syncPlanItem{{Action: "conflict"}, {Action: "conflict"}}}
	if err := validateSyncCheck(conflicted); err == nil || !strings.Contains(err.Error(), "2 unresolved conflict(s)") {
		t.Fatalf("conflicted plan passed sync check: %v", err)
	}
}

func TestResolveSyncPlanOptionsRequiresOneWayDirectionForDelete(t *testing.T) {
	if _, err := resolveSyncPlanOptionsWithDelete("both", "error", true); err == nil || !strings.Contains(err.Error(), "requires explicit --direction upload or download") {
		t.Fatalf("two-way --delete was accepted: %v", err)
	}
	for _, direction := range []string{syncDirectionUpload, syncDirectionDownload} {
		options, err := resolveSyncPlanOptionsWithDelete(direction, syncConflictError, true)
		if err != nil {
			t.Fatalf("one-way --delete direction %q rejected: %v", direction, err)
		}
		if !options.DeleteExtraneous || options.Direction != direction {
			t.Fatalf("delete options not preserved for %q: %#v", direction, options)
		}
	}
}

func TestBuildSyncPlanUploadDeleteCollapsesRemoteOnlyDirectory(t *testing.T) {
	localRoot := t.TempDir()
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {
				{FileID: "orphan-file", Name: "orphan.bin", Size: 4, Sha1: testSyncSHA1("file")},
				{FileID: "orphan-dir", Name: "orphan-dir", IsDirectory: true},
			},
			"orphan-dir": {{FileID: "orphan-child", Name: "child.bin", Size: 5, Sha1: testSyncSHA1("child")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError, DeleteExtraneous: true})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	if item := actions["orphan.bin"]; item.Action != "delete-remote" || item.Reason != "mirror-delete:remote-only" || !item.Destructive {
		t.Fatalf("remote-only file was not planned for mirror deletion: %#v", item)
	}
	if item := actions["orphan-dir"]; item.Action != "delete-remote" || item.Kind != "directory" || !item.Destructive {
		t.Fatalf("remote-only directory root was not planned for deletion: %#v", item)
	}
	if child := actions["orphan-dir/child.bin"]; child.Action != "skip" || child.Reason != "covered-by-delete-remote:orphan-dir" {
		t.Fatalf("remote delete subtree was not collapsed: %#v", child)
	}
	if !plan.Ready || !plan.DeleteExtraneous || !plan.RequiresAllowDestructive || len(plan.PlanID) != 64 || plan.DestructiveActions != 2 || plan.DeleteRemoteRoots != 2 || plan.DeleteRemoteFiles != 2 || plan.DeleteRemoteDirs != 1 || plan.DeleteRemoteBytes != 9 || plan.CoveredByDelete != 1 || plan.DownloadFiles != 0 {
		t.Fatalf("unexpected upload mirror-delete summary: %#v", plan)
	}
}

func TestBuildSyncPlanDownloadDeleteCollapsesLocalOnlyDirectory(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "orphan.bin", "file")
	writeSyncTestFile(t, localRoot, "orphan-dir/child.bin", "child")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionDownload, ConflictPolicy: syncConflictError, DeleteExtraneous: true})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	if item := actions["orphan.bin"]; item.Action != "delete-local" || item.Reason != "mirror-delete:local-only" || !item.Destructive {
		t.Fatalf("local-only file was not planned for mirror deletion: %#v", item)
	}
	if item := actions["orphan-dir"]; item.Action != "delete-local" || item.Kind != "directory" || !item.Destructive {
		t.Fatalf("local-only directory root was not planned for deletion: %#v", item)
	}
	if child := actions["orphan-dir/child.bin"]; child.Action != "skip" || child.Reason != "covered-by-delete-local:orphan-dir" {
		t.Fatalf("local delete subtree was not collapsed: %#v", child)
	}
	if !plan.Ready || !plan.DeleteExtraneous || !plan.RequiresAllowDestructive || len(plan.PlanID) != 64 || plan.DestructiveActions != 2 || plan.DeleteLocalRoots != 2 || plan.DeleteLocalFiles != 2 || plan.DeleteLocalDirs != 1 || plan.DeleteLocalBytes != 9 || plan.CoveredByDelete != 1 || plan.UploadFiles != 0 {
		t.Fatalf("unexpected download mirror-delete summary: %#v", plan)
	}
}

func TestSyncPlanFingerprintIsDeterministicAndSnapshotSensitive(t *testing.T) {
	plan := syncPlan{
		Mode: syncPlanMode, Direction: syncDirectionUpload, ConflictPolicy: syncConflictError, DeleteExtraneous: true,
		LocalRoot: "C:/data", RemoteRoot: "/remote", RemoteRootID: "root-id",
		Items: []syncPlanItem{{
			RelativePath: "a.bin", Action: "delete-remote", Kind: "file", Reason: "mirror-delete:remote-only",
			RemotePresent: true, LocalPath: "C:/data/a.bin", RemotePath: "/remote/a.bin", RemoteID: "file-id", RemoteSize: 4, RemoteSHA1: testSyncSHA1("file"), Destructive: true,
			RemoteModTimeUnixNano: 123,
		}},
	}
	first := syncPlanFingerprint(plan)
	second := syncPlanFingerprint(plan)
	if len(first) != 64 || first != second {
		t.Fatalf("plan fingerprint is not deterministic SHA-256: first=%q second=%q", first, second)
	}
	mutations := []func(*syncPlan){
		func(p *syncPlan) { p.Items[0].RemoteID = "other-id" },
		func(p *syncPlan) { p.Items[0].RemoteSize++ },
		func(p *syncPlan) { p.Items[0].RemoteModTimeUnixNano++ },
		func(p *syncPlan) { p.Items[0].Action = "skip" },
		func(p *syncPlan) { p.DeleteExtraneous = false },
	}
	for index, mutate := range mutations {
		copyPlan := plan
		copyPlan.Items = append([]syncPlanItem(nil), plan.Items...)
		mutate(&copyPlan)
		if got := syncPlanFingerprint(copyPlan); got == first {
			t.Fatalf("fingerprint mutation %d did not change plan ID", index)
		}
	}
}

func TestValidateSyncExpectedPlanID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	normalized, err := normalizeSyncPlanID(strings.ToUpper(id))
	if err != nil || normalized != id {
		t.Fatalf("plan ID normalization failed: got=%q err=%v", normalized, err)
	}
	if _, err := normalizeSyncPlanID("xyz"); err == nil {
		t.Fatal("invalid expected plan ID was accepted")
	}
	plan := syncPlan{PlanID: id}
	if err := validateSyncExpectedPlanID(plan, normalized); err != nil {
		t.Fatal(err)
	}
	other := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := validateSyncExpectedPlanID(plan, other); err == nil || !strings.Contains(err.Error(), "plan ID mismatch") {
		t.Fatalf("mismatched reviewed plan ID was accepted: %v", err)
	}
}

func TestBuildSyncPlanUploadDirectionPreservesRemoteOnlyEntries(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "local.bin", "local")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote-only", Name: "remote.bin", Size: 6, Sha1: testSyncSHA1("remote")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	if actions["local.bin"].Action != "upload" || actions["remote.bin"].Action != "skip" || actions["remote.bin"].Reason != "direction-excludes-download" {
		t.Fatalf("unexpected upload-only actions: %#v", actions)
	}
	if !plan.Ready || plan.Direction != syncDirectionUpload || plan.UploadFiles != 1 || plan.DownloadFiles != 0 || plan.SkippedFiles != 1 || plan.Conflicts != 0 {
		t.Fatalf("unexpected upload-only summary: %#v", plan)
	}
}

func TestBuildSyncPlanDownloadDirectionPreservesLocalOnlyEntries(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "local.bin", "local")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote-only", Name: "remote.bin", Size: 6, Sha1: testSyncSHA1("remote")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionDownload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	if actions["local.bin"].Action != "skip" || actions["local.bin"].Reason != "direction-excludes-upload" || actions["remote.bin"].Action != "download" {
		t.Fatalf("unexpected download-only actions: %#v", actions)
	}
	if !plan.Ready || plan.Direction != syncDirectionDownload || plan.UploadFiles != 0 || plan.DownloadFiles != 1 || plan.SkippedFiles != 1 || plan.Conflicts != 0 {
		t.Fatalf("unexpected download-only summary: %#v", plan)
	}
}

func TestBuildSyncPlanPreferLocalResolvesFileConflictAsDestructiveReplace(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "conflict.bin", "aaaa")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "conflict", Name: "conflict.bin", Size: 4, Sha1: testSyncSHA1("bbbb")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["conflict.bin"]
	if item.Action != "replace-remote" || !item.Destructive || item.ReplacesKind != "file" || item.Reason != "prefer-local:sha1-mismatch" {
		t.Fatalf("unexpected prefer-local replacement: %#v", item)
	}
	if !plan.Ready || plan.Conflicts != 0 || plan.ResolvedConflicts != 1 || plan.DestructiveActions != 1 || plan.UploadFiles != 1 || plan.UploadBytes != 4 || plan.DownloadFiles != 0 || plan.ChecksummedFiles != 1 {
		t.Fatalf("unexpected prefer-local summary: %#v", plan)
	}
}

func TestBuildSyncPlanPreferRemoteResolvesFileConflictAsDestructiveReplace(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "conflict.bin", "aaaa")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "conflict", Name: "conflict.bin", Size: 4, Sha1: testSyncSHA1("bbbb")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictRemote})
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["conflict.bin"]
	if item.Action != "replace-local" || !item.Destructive || item.ReplacesKind != "file" || item.Reason != "prefer-remote:sha1-mismatch" {
		t.Fatalf("unexpected prefer-remote replacement: %#v", item)
	}
	if !plan.Ready || plan.Conflicts != 0 || plan.ResolvedConflicts != 1 || plan.DestructiveActions != 1 || plan.DownloadFiles != 1 || plan.DownloadBytes != 4 || plan.UploadFiles != 0 || plan.ChecksummedFiles != 1 {
		t.Fatalf("unexpected prefer-remote summary: %#v", plan)
	}
}

func TestBuildSyncPlanPreferLocalTypeReplacementKeepsWinningDirectorySubtree(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "node/child.bin", "child")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "remote-node", Name: "node", Size: 2, Sha1: testSyncSHA1("xx")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	root := actions["node"]
	if root.Action != "replace-remote" || root.Kind != "directory" || root.ReplacesKind != "file" || !root.Destructive {
		t.Fatalf("unexpected root type replacement: %#v", root)
	}
	if child := actions["node/child.bin"]; child.Action != "upload" || child.Reason != "local-only" {
		t.Fatalf("winning local directory subtree was not retained: %#v", child)
	}
	if !plan.Ready || plan.Conflicts != 0 || plan.ResolvedConflicts != 1 || plan.DestructiveActions != 1 || plan.UploadDirs != 1 || plan.UploadFiles != 1 || plan.DownloadFiles != 0 {
		t.Fatalf("unexpected prefer-local directory replacement summary: %#v", plan)
	}
}

func TestBuildSyncPlanPreferLocalFileReplacementCoversLosingRemoteSubtree(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "node", "local-file")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root":        {{FileID: "remote-node", Name: "node", IsDirectory: true}},
			"remote-node": {{FileID: "remote-child", Name: "child.bin", Size: 5, Sha1: testSyncSHA1("child")}},
		},
		files: map[string]driver.File{},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	actions := syncActionsByPath(plan)
	root := actions["node"]
	if root.Action != "replace-remote" || root.Kind != "file" || root.ReplacesKind != "directory" || !root.Destructive {
		t.Fatalf("unexpected file-over-directory replacement: %#v", root)
	}
	child := actions["node/child.bin"]
	if child.Action != "skip" || child.Reason != "covered-by-replace-remote:node" {
		t.Fatalf("losing remote subtree leaked an action: %#v", child)
	}
	if !plan.Ready || plan.UploadFiles != 1 || plan.DownloadFiles != 0 || plan.DownloadDirs != 0 || plan.ResolvedConflicts != 1 || plan.DestructiveActions != 1 {
		t.Fatalf("unexpected covered subtree summary: %#v", plan)
	}
}

func TestBuildSyncPlanPreferLocalUnavailableRemoteSHA1RemainsConflict(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "unknown.bin", "abc")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{
			"root": {{FileID: "unknown", Name: "unknown.bin", Size: 3}},
		},
		files: map[string]driver.File{
			"unknown": {FileID: "unknown", Name: "unknown.bin", Size: 3},
		},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictLocal})
	if err != nil {
		t.Fatal(err)
	}
	item := syncActionsByPath(plan)["unknown.bin"]
	if item.Action != "conflict" || item.Reason != "remote-sha1-unavailable" || item.Destructive {
		t.Fatalf("unavailable remote SHA1 was not left unresolved: %#v", item)
	}
	if plan.Ready || plan.ChecksummedFiles != 0 || plan.ChecksummedBytes != 0 || item.LocalSHA1 != "" || item.LocalPreparedDigest != nil || client.getFileCalls != 1 || plan.Conflicts != 1 || plan.ResolvedConflicts != 0 || plan.DestructiveActions != 0 {
		t.Fatalf("unexpected unavailable-SHA1 conflict plan: plan=%#v calls=%d", plan, client.getFileCalls)
	}
}
