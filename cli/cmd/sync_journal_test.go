package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

func testSyncJournalPlan(t *testing.T) syncPlan {
	t.Helper()
	root := t.TempDir()
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictError, Ready: true, LocalRoot: root, RemoteRoot: "/remote", RemoteRootID: "root-id",
		Items: []syncPlanItem{{
			RelativePath: "file.bin", Action: "upload", Kind: "file", Reason: "local-only",
			LocalPresent: true, LocalPath: root + string(os.PathSeparator) + "file.bin", RemotePath: "/remote/file.bin",
			LocalSize: 4, LocalModTimeUnixNano: 123456789,
		}},
	}
	plan.ChangeActions = syncPlanChangeCount(plan)
	plan.PlanID = syncPlanFingerprint(plan)
	return plan
}

func testSyncJournalStore(t *testing.T) syncJournalStore {
	t.Helper()
	return syncJournalStore{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
}

func TestSyncJournalCreateOpenRestoresPrivatePlanSnapshot(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if handle.journal.PlanID != plan.PlanID || len(handle.journal.Items) != 1 || handle.journal.Items[0].State != "pending" {
		t.Fatalf("unexpected created journal: %#v", handle.journal)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(plan.PlanID[:12])
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if got := opened.journal.Plan.Items[0].LocalModTimeUnixNano; got != 123456789 {
		t.Fatalf("private local snapshot was not restored: %d", got)
	}
	if fingerprint := syncPlanFingerprint(opened.journal.Plan); fingerprint != plan.PlanID {
		t.Fatalf("restored plan fingerprint: got %s want %s", fingerprint, plan.PlanID)
	}
}

func TestSyncJournalCreateRefusesExistingJournalAndCrossAccountRead(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(plan); err == nil || !errors.Is(err, errSyncJournalExists) {
		t.Fatalf("existing journal was not rejected: %v", err)
	}
	other := store
	other.AccountID = 99
	if _, err := other.Inspect(plan.PlanID); err == nil || !strings.Contains(err.Error(), "account 42") {
		t.Fatalf("cross-account journal read was accepted: %v", err)
	}
}

func TestValidateSyncJournalAccountBindingFailsClosed(t *testing.T) {
	if err := validateSyncJournalAccountBinding(syncJournalStore{}); err == nil || !strings.Contains(err.Error(), "known 115 account") {
		t.Fatalf("unknown account identity was accepted for sync journal execution: %v", err)
	}
	if err := validateSyncJournalAccountBinding(syncJournalStore{AccountID: 42}); err != nil {
		t.Fatalf("known account identity was rejected: %v", err)
	}
}

func TestResolveSyncJournalStoreIgnoresUnrelatedInvalidTransferSettings(t *testing.T) {
	oldConfigPath, oldProfile, oldClient := configPath, profile, client
	defer func() { configPath, profile, client = oldConfigPath, oldProfile, oldClient }()
	root := t.TempDir()
	sessionRoot := filepath.Join(root, "sessions")
	configPath = filepath.Join(root, "config.toml")
	profile = "main"
	client = nil
	t.Setenv("115DRIVER_SESSION_DIR", "")
	contents := "[transfer]\ninterfaces = \"\"\nworkers_per_interface = 0\nchunk_size = \"\"\n\n[transfer.sessions]\ndir = \"" + filepath.ToSlash(sessionRoot) + "\"\n"
	if err := os.WriteFile(configPath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := resolveSyncJournalStore()
	if err != nil {
		t.Fatalf("journal store inherited unrelated transfer config failure: %v", err)
	}
	if filepath.Clean(store.Root) != filepath.Clean(sessionRoot) {
		t.Fatalf("journal store root: got %q want %q", store.Root, sessionRoot)
	}
	if len(store.ProfileScope) != 64 {
		t.Fatalf("journal store profile scope length: %d", len(store.ProfileScope))
	}
	if !store.AutoGC || store.GCInterval <= 0 || store.Retention <= 0 || store.TrashRetention <= 0 {
		t.Fatalf("journal store did not inherit session maintenance settings: %#v", store)
	}
}

func TestSyncJournalLocationUsesFullProfileScope(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, err := store.location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	clean := filepath.ToSlash(location.Dir)
	if !strings.Contains(clean, "/"+store.ProfileScope+"/") {
		t.Fatalf("journal location does not use full profile scope: %s", clean)
	}
	if strings.Contains(clean, "/"+store.ProfileScope[:16]+"/") {
		t.Fatalf("journal location unexpectedly uses truncated profile scope: %s", clean)
	}
}

func TestSyncJournalListRejectsCorruptJournal(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := handle.location.JournalPath
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil || !strings.Contains(err.Error(), plan.PlanID) {
		t.Fatalf("corrupt sync journal was silently skipped: %v", err)
	}
}

func TestSyncJournalSnapshotIsDetached(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.Items[0].Post = &syncJournalPostcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "remote-id"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := handle.snapshot()
	snapshot.Items[0].State = "changed-outside-lock"
	snapshot.Items[0].Post.RemoteID = "changed"
	snapshot.Plan.Items[0].Action = "download"
	fresh := handle.snapshot()
	if fresh.Items[0].State == "changed-outside-lock" || fresh.Items[0].Post.RemoteID != "remote-id" || fresh.Plan.Items[0].Action != "upload" {
		t.Fatalf("journal snapshot shares mutable backing state: %#v", fresh)
	}
}

func TestSyncJournalMutateWriteFailureDoesNotPublishInMemoryState(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	before := handle.snapshot()
	blocker := filepath.Join(handle.location.Dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	handle.location.JournalPath = filepath.Join(blocker, "journal.json")
	err = handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.LastError = "must-not-publish"
		journal.Items[0].State = "failed"
		return nil
	})
	if err == nil {
		t.Fatal("journal mutation unexpectedly succeeded through a non-directory parent")
	}
	after := handle.snapshot()
	if after.State != before.State || after.LastError != before.LastError || after.Items[0].State != before.Items[0].State {
		t.Fatalf("failed journal write leaked into in-memory state: before=%#v after=%#v", before, after)
	}
}

func TestSyncJournalReconcileRequiredIsProtectedFromGCAndNormalTrash(t *testing.T) {
	store := testSyncJournalStore(t)
	store.Retention = time.Nanosecond
	plan := testSyncJournalPlan(t)
	plan.Items[0] = syncPlanItem{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", RemotePresent: true, RemotePath: "/remote/old.bin", RemoteID: "old", Destructive: true}
	plan.DestructiveActions = 1
	plan.ChangeActions = 1
	plan.PlanID = syncPlanFingerprint(plan)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "delete-started"
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 || !entries[0].ReconcileRequired || entries[0].RecoveryRequired {
		t.Fatalf("interrupted destructive journal not classified for reconciliation: entries=%#v err=%v", entries, err)
	}
	time.Sleep(2 * time.Millisecond)
	actions, err := store.GC(0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("GC proposed deleting reconcile-required journal: %#v", actions)
	}
	if _, err := store.Trash(plan.PlanID); err == nil || !errors.Is(err, errSyncJournalRecoveryRemoval) {
		t.Fatalf("normal trash removed reconcile-required journal: %v", err)
	}
	if _, err := store.ForceTrash(plan.PlanID); err != nil {
		t.Fatalf("forced cleanup after review failed: %v", err)
	}
}

func TestSyncJournalRecoveryEvidenceRequiresForceToTrash(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	plan.Items[0] = syncPlanItem{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", RemotePresent: true, RemotePath: "/remote/old.bin", RemoteID: "old", Destructive: true}
	plan.DestructiveActions = 1
	plan.ChangeActions = 1
	plan.PlanID = syncPlanFingerprint(plan)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "recovery-required"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "delete-started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Trash(plan.PlanID); err == nil || !errors.Is(err, errSyncJournalRecoveryRemoval) {
		t.Fatalf("recovery evidence was removable without --force semantics: %v", err)
	}
	trash, err := store.ForceTrash(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("forced recovery journal trash is missing: %v", err)
	}
}

func TestSyncJournalListTrashAndGC(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "completed"
		journal.Items[0].State = "succeeded"
		journal.Items[0].Phase = "done"
		journal.Items[0].Post = &syncJournalPostcondition{Side: "remote", Exists: true, Kind: "file", Size: 4}
		old := time.Now().UTC().Add(-48 * time.Hour)
		journal.Items[0].UpdatedAt = old
		journal.UpdatedAt = old
		journal.CreatedAt = old
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	shared := store.sharedCurrentStore()
	reviewID := "sha256:" + strings.Repeat("a", 64)
	if _, err := shared.WriteReviewAlias(reviewID, plan.PlanID); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 || entries[0].Completed != 1 {
		t.Fatalf("unexpected journal list: entries=%#v err=%v", entries, err)
	}
	// mutate() intentionally refreshes UpdatedAt on write; verify GC uses the store/session retention when no override is supplied.
	store.Retention = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	actions, err := store.GC(0, true)
	if err != nil || len(actions) != 1 || actions[0].PlanID != plan.PlanID {
		t.Fatalf("unexpected dry-run journal GC: %#v err=%v", actions, err)
	}
	trash, err := store.Trash(plan.PlanID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("trashed journal is missing: %v", err)
	}
	if _, err := store.Inspect(plan.PlanID); !errors.Is(err, errSyncJournalNotFound) {
		t.Fatalf("trashed journal remained inspectable: %v", err)
	}
	aliases, found, err := syncjournalpkg.ReadTrashReviewAliases(trash)
	if err != nil || !found || len(aliases) != 1 || aliases[0] != reviewID {
		t.Fatalf("CLI trash did not preserve reviewed alias sidecar: aliases=%v found=%v err=%v", aliases, found, err)
	}
	if _, err := shared.ResolveReviewAlias(reviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("CLI trash left current review alias behind: %v", err)
	}
	record, err := resolveSyncJournalTrashRecord(shared, plan.PlanID[:12])
	if err != nil || record.Journal.PlanID != plan.PlanID {
		t.Fatalf("resolve trashed journal record=%#v err=%v", record, err)
	}
	guard, err := shared.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := shared.RestoreTrashedCurrent(guard, record.TrashName, record.Journal.PlanID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs)
	closeErr := guard.Close()
	if err != nil || closeErr != nil || restored.PlanID != plan.PlanID {
		t.Fatalf("restore trashed journal restored=%#v err=%v close=%v", restored, err, closeErr)
	}
	if current, err := store.Inspect(plan.PlanID[:12]); err != nil || current.PlanID != plan.PlanID {
		t.Fatalf("restored raw journal is not inspectable: current=%#v err=%v", current, err)
	}
	if resolved, err := shared.ResolveReviewAlias(reviewID); err != nil || resolved != plan.PlanID {
		t.Fatalf("CLI raw restore did not recreate reviewed alias: resolved=%q err=%v", resolved, err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("restored trash directory still exists: %v", err)
	}
}

func TestSyncJournalDestructivePhaseRequiresReconciliationBeforeManualRecovery(t *testing.T) {
	plan := testSyncJournalPlan(t)
	plan.Items[0].Action = "delete-remote"
	plan.Items[0].Destructive = true
	plan.Items[0].LocalPresent = false
	plan.Items[0].RemotePresent = true
	plan.PlanID = syncPlanFingerprint(plan)
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("b", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "failed"
	journal.Items[0].Phase = "delete-started"
	if syncJournalRecoveryRequired(journal) {
		t.Fatal("interrupted destructive phase was prematurely marked manual recovery-required")
	}
	if !syncJournalDestructiveReconciliationRequired(journal) {
		t.Fatal("interrupted destructive phase did not request state reconciliation")
	}
	journal.State = "recovery-required"
	if !syncJournalRecoveryRequired(journal) {
		t.Fatal("explicit manual recovery state was not recognized")
	}
	journal.State = "failed"
	journal.Items[0].State = "succeeded"
	journal.Items[0].Phase = "done"
	if syncJournalDestructiveReconciliationRequired(journal) {
		t.Fatal("completed destructive item still requested reconciliation")
	}
}

func TestSyncJournalExecutionPersistsPhaseBeforeMutationAndPostconditionAfter(t *testing.T) {
	localRoot := t.TempDir()
	localDir := filepath.Join(localRoot, "created")
	if err := os.Mkdir(localDir, 0755); err != nil {
		t.Fatal(err)
	}
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictError, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{{RelativePath: "created", Action: "upload", Kind: "directory", Reason: "local-only", LocalPresent: true, LocalPath: localDir, RemotePath: "/remote/created"}},
	}
	plan.ChangeActions = 1
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	client := &syncReadOnlyClient{dirIDs: map[string]string{"remote": "root"}, lists: map[string][]driver.File{"root": {}}, files: map[string]driver.File{}}
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error {
			if got := handle.snapshot().Items[0].Phase; got != "mutation-started" {
				t.Fatalf("journal phase before mutation: got %q", got)
			}
			client.lists["root"] = []driver.File{{FileID: "d-created", Name: "created", IsDirectory: true}}
			return nil
		},
	}
	deps = attachSyncJournalExecutionDeps(handle, client, deps)
	summary, err := executeSyncPlan(context.Background(), plan, false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.finishRun(summary, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := handle.snapshot()
	item := snapshot.Items[0]
	if snapshot.State != "completed" || item.State != "succeeded" || item.Phase != "done" || item.Attempts != 1 || item.Post == nil || item.Post.RemoteID != "d-created" {
		t.Fatalf("unexpected completed journal lifecycle: %#v", snapshot)
	}
}

func TestSyncJournalParallelExecutionPersistsConsistentItems(t *testing.T) {
	localRoot := t.TempDir()
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionDownload,
		ConflictPolicy: syncConflictError, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: make([]syncPlanItem, 0, 12),
	}
	for index := 0; index < 12; index++ {
		name := fmt.Sprintf("dir-%02d", index)
		plan.Items = append(plan.Items, syncPlanItem{
			RelativePath: name, Action: "download", Kind: "directory", Reason: "remote-only",
			RemotePresent: true, LocalPath: filepath.Join(localRoot, name), RemotePath: "/remote/" + name, RemoteID: "remote-" + name,
		})
	}
	plan.ChangeActions = syncPlanChangeCount(plan)
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	deps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		createLocalDirectory: func(_ context.Context, item syncPlanItem) error {
			return os.Mkdir(item.LocalPath, 0755)
		},
	}
	deps = attachSyncJournalExecutionDeps(handle, nil, deps)
	summary, err := executeSyncPlanWithJobs(context.Background(), plan, false, 4, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.finishRun(summary, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := handle.snapshot()
	if snapshot.State != "completed" || len(snapshot.Items) != len(plan.Items) {
		t.Fatalf("parallel journal final state: %#v", snapshot)
	}
	for _, item := range snapshot.Items {
		if item.State != "succeeded" || item.Phase != "done" || item.Attempts != 1 || item.Post == nil || item.Post.Side != "local" || !item.Post.Exists || item.Post.Kind != "directory" {
			t.Fatalf("parallel journal item is inconsistent: %#v", item)
		}
	}
	persisted, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatalf("parallel journal JSON could not be read back: %v", err)
	}
	if persisted.State != "completed" || len(persisted.Items) != len(plan.Items) {
		t.Fatalf("persisted parallel journal differs from memory: %#v", persisted)
	}
}

func TestSyncJournalResumeReconcilesCrashAfterNonDestructiveUpload(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "file.bin", "data")
	info, err := os.Lstat(filepath.Join(localRoot, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictError, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{{
			RelativePath: "file.bin", Action: "upload", Kind: "file", Reason: "local-only", LocalPresent: true,
			LocalPath: filepath.Join(localRoot, "file.bin"), RemotePath: "/remote/file.bin", LocalSize: 4, LocalModTimeUnixNano: info.ModTime().UnixNano(),
		}},
	}
	plan.ChangeActions = 1
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "mutation-started"
		journal.Items[0].LastError = "simulated process loss"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sha := testSyncSHA1("data")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}}},
		files:  map[string]driver.File{"remote-file": {FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}},
	}
	if err := handle.reconcileForResume(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	item := handle.snapshot().Items[0]
	if item.State != "succeeded" || item.Phase != "done" || item.Post == nil || item.Post.RemoteID != "remote-file" || !strings.EqualFold(item.Post.SHA1, sha) {
		t.Fatalf("crash-after-upload was not reconciled as completed: %#v", item)
	}
}

func TestSyncJournalTwoRunResumeExecutesOnlyRemainingBranch(t *testing.T) {
	localRoot := t.TempDir()
	localA := filepath.Join(localRoot, "a")
	localB := filepath.Join(localRoot, "b")
	if err := os.Mkdir(localA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(localB, 0755); err != nil {
		t.Fatal(err)
	}
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictError, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{
			{RelativePath: "a", Action: "upload", Kind: "directory", Reason: "local-only", LocalPresent: true, LocalPath: localA, RemotePath: "/remote/a"},
			{RelativePath: "b", Action: "upload", Kind: "directory", Reason: "local-only", LocalPresent: true, LocalPath: localB, RemotePath: "/remote/b"},
		},
	}
	plan.ChangeActions = syncPlanChangeCount(plan)
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}

	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.beginRun(false); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	firstCalls := make([]string, 0, 2)
	firstDeps := syncExecutionDeps{
		preflight: func(context.Context) error { return nil },
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error {
			firstCalls = append(firstCalls, item.RelativePath)
			if item.RelativePath == "b" {
				return errors.New("simulated first-run branch failure")
			}
			client.lists["root"] = append(client.lists["root"], driver.File{FileID: "d-a", Name: "a", IsDirectory: true})
			client.lists["d-a"] = []driver.File{}
			return nil
		},
	}
	firstDeps = attachSyncJournalExecutionDeps(handle, client, firstDeps)
	firstSummary, firstErr := executeSyncPlanWithJobs(context.Background(), plan, false, 1, firstDeps)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "b") {
		_ = handle.Close()
		t.Fatalf("first run did not fail on the second branch: %v", firstErr)
	}
	if err := handle.finishRun(firstSummary, firstErr); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	firstSnapshot := handle.snapshot()
	if len(firstCalls) != 2 || firstCalls[0] != "a" || firstCalls[1] != "b" || firstSnapshot.State != syncJournalStatusFailed {
		_ = handle.Close()
		t.Fatalf("first run lifecycle: calls=%#v journal=%#v", firstCalls, firstSnapshot)
	}
	if firstSnapshot.Items[0].State != "succeeded" || firstSnapshot.Items[0].Attempts != 1 || firstSnapshot.Items[0].Post == nil || firstSnapshot.Items[0].Post.RemoteID != "d-a" {
		_ = handle.Close()
		t.Fatalf("successful branch was not durably recorded: %#v", firstSnapshot.Items[0])
	}
	if firstSnapshot.Items[1].State != "failed" || firstSnapshot.Items[1].Attempts != 1 || firstSnapshot.Items[1].Phase != "mutation-started" {
		_ = handle.Close()
		t.Fatalf("failed branch evidence was not durably recorded: %#v", firstSnapshot.Items[1])
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := openSyncResumeJournal(store, plan.PlanID[:12], plan.LocalRoot, plan.RemoteRoot, plan.PlanID, syncDeleteBudget{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err := resumed.reconcileForResume(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	reconciled := resumed.snapshot()
	if reconciled.Items[0].State != "succeeded" || reconciled.Items[1].State != "pending" || reconciled.Items[1].Phase != "mutation-started" {
		t.Fatalf("resume reconciliation did not preserve completed/pending split: %#v", reconciled.Items)
	}
	if err := preflightSyncJournalResume(context.Background(), client, reconciled); err != nil {
		t.Fatalf("two-run resume preflight failed: %v", err)
	}
	residual := buildSyncJournalResidualPlan(reconciled)
	if residual.Items[0].Action != "skip" || residual.Items[1].Action != "upload" || residual.ChangeActions != 1 {
		t.Fatalf("unexpected two-run residual plan: %#v", residual.Items)
	}
	if err := resumed.beginRun(true); err != nil {
		t.Fatal(err)
	}
	secondCalls := make([]string, 0, 1)
	secondDeps := syncExecutionDeps{
		forcePreflight: true,
		preflight: func(ctx context.Context) error {
			return preflightSyncJournalResume(ctx, client, resumed.snapshot())
		},
		createRemoteDirectory: func(_ context.Context, item syncPlanItem) error {
			secondCalls = append(secondCalls, item.RelativePath)
			if item.RelativePath != "b" {
				return fmt.Errorf("completed branch %q was executed again", item.RelativePath)
			}
			client.lists["root"] = append(client.lists["root"], driver.File{FileID: "d-b", Name: "b", IsDirectory: true})
			client.lists["d-b"] = []driver.File{}
			return nil
		},
	}
	secondDeps = attachSyncJournalExecutionDeps(resumed, client, secondDeps)
	secondSummary, secondErr := executeSyncPlanWithJobs(context.Background(), residual, false, 1, secondDeps)
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	if err := resumed.finishRun(secondSummary, nil); err != nil {
		t.Fatal(err)
	}
	final := resumed.snapshot()
	if len(secondCalls) != 1 || secondCalls[0] != "b" {
		t.Fatalf("resume execution did not isolate remaining branch: %#v", secondCalls)
	}
	if secondSummary.Processed != 2 || secondSummary.Skipped != 1 || secondSummary.Succeeded != 1 {
		t.Fatalf("resume execution summary: %#v", secondSummary)
	}
	if final.State != syncJournalStatusCompleted || final.Status != syncJournalStatusCompleted || final.Items[0].Attempts != 1 || final.Items[1].Attempts != 2 {
		t.Fatalf("two-run journal did not converge: %#v", final)
	}
	if final.Items[0].Post == nil || final.Items[0].Post.RemoteID != "d-a" || final.Items[1].Post == nil || final.Items[1].Post.RemoteID != "d-b" {
		t.Fatalf("two-run journal postconditions: %#v", final.Items)
	}
	if final.RunStats.Runs != 2 || final.RunStats.ResumeRuns != 1 || final.RunStats.InterruptedRuns != 0 {
		t.Fatalf("two-run journal run stats: %#v", final.RunStats)
	}
	persisted, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != syncJournalStatusCompleted || persisted.Items[0].Attempts != 1 || persisted.Items[1].Attempts != 2 {
		t.Fatalf("persisted two-run journal did not match memory: %#v", persisted)
	}
}

func TestSyncJournalResumePreservesInterruptedDownloadArtifactsThroughReconcile(t *testing.T) {
	localRoot := t.TempDir()
	sha := testSyncSHA1("data")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}}},
		files:  map[string]driver.File{"remote-file": {FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionDownload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "mutation-started"
		journal.Items[0].LastError = "simulated interruption"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writeSyncTestFile(t, localRoot, ".file.bin.115driver.part", "part")
	if err := handle.reconcileForResume(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	snapshot := handle.snapshot()
	if snapshot.Items[0].State != "pending" || snapshot.Items[0].Phase != "mutation-started" {
		t.Fatalf("download interruption phase was lost during reconcile: %#v", snapshot.Items[0])
	}
	if err := preflightSyncJournalResume(context.Background(), client, snapshot); err != nil {
		t.Fatalf("reconciled interrupted download artifact failed resume preflight: %v", err)
	}
}

func TestSyncJournalProductionDependencyInitFailureMarksJournalFailed(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	oldClient := client
	client = nil
	defer func() { client = oldClient }()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err = runSyncExecutionWithJournalHandle(cmd, plan, false, 1, false, 0, handle, false)
	if err == nil || !strings.Contains(err.Error(), "initialize sync execution") {
		t.Fatalf("expected production dependency initialization failure, got %v", err)
	}
	exitErr, ok := err.(*exitError)
	if !ok {
		t.Fatalf("dependency initialization failure did not return exitError: %T", err)
	}
	summary, ok := exitErr.data.(syncExecutionSummary)
	if !ok || !summary.JournalEnabled || summary.JournalResumed || summary.JournalVersion != syncJournalVersion || summary.JournalState != syncJournalStatusFailed || summary.JournalStatus != syncJournalStatusFailed {
		t.Fatalf("dependency initialization failure omitted journal machine state: %#v", exitErr.data)
	}
	snapshot := handle.snapshot()
	if snapshot.State != syncJournalStatusFailed || snapshot.Status != syncJournalStatusFailed || !strings.Contains(snapshot.LastError, "sync client is nil") {
		t.Fatalf("journal remained active after dependency initialization failure: %#v", snapshot)
	}
	if snapshot.RunStats.Runs != 1 || snapshot.RunStats.LastStartedAt == nil || snapshot.RunStats.LastFinishedAt == nil {
		t.Fatalf("dependency initialization failure did not close run stats: %#v", snapshot.RunStats)
	}
}

func TestSyncJournalResumeReconcilesDestructiveDeleteEvidence(t *testing.T) {
	newDeleteJournal := func(t *testing.T) (syncPlan, syncJournalStore, *syncJournalHandle) {
		t.Helper()
		plan := testSyncJournalPlan(t)
		plan.Items[0] = syncPlanItem{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", Reason: "mirror-delete:remote-only", RemotePresent: true, RemotePath: "/remote/old.bin", RemoteID: "old", RemoteSize: 4, RemoteSHA1: testSyncSHA1("old!"), Destructive: true}
		plan.DestructiveActions = 1
		plan.ChangeActions = 1
		plan.PlanID = syncPlanFingerprint(plan)
		store := testSyncJournalStore(t)
		handle, err := store.Create(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.mutate(func(journal *syncExecutionJournal) error {
			journal.State = "failed"
			journal.Items[0].State = "failed"
			journal.Items[0].Phase = "delete-started"
			return nil
		}); err != nil {
			handle.Close()
			t.Fatal(err)
		}
		return plan, store, handle
	}

	t.Run("original-loser-still-present-retries-full-delete", func(t *testing.T) {
		_, _, handle := newDeleteJournal(t)
		defer handle.Close()
		sha := testSyncSHA1("old!")
		client := &syncReadOnlyClient{
			dirIDs: map[string]string{"remote": "root"},
			lists:  map[string][]driver.File{"root": {{FileID: "old", Name: "old.bin", Size: 4, Sha1: sha}}},
			files:  map[string]driver.File{"old": {FileID: "old", Name: "old.bin", Size: 4, Sha1: sha}},
		}
		if err := handle.reconcileForResume(context.Background(), client); err != nil {
			t.Fatal(err)
		}
		item := handle.snapshot().Items[0]
		if item.State != "pending" || item.Phase != "pending" {
			t.Fatalf("unchanged loser was not reset for safe full retry: %#v", item)
		}
	})

	t.Run("already-absent-target-is-reconciled-as-completed", func(t *testing.T) {
		_, _, handle := newDeleteJournal(t)
		defer handle.Close()
		client := &syncReadOnlyClient{
			dirIDs: map[string]string{"remote": "root"},
			lists:  map[string][]driver.File{"root": {}},
			files:  map[string]driver.File{},
		}
		if err := handle.reconcileForResume(context.Background(), client); err != nil {
			t.Fatal(err)
		}
		item := handle.snapshot().Items[0]
		if item.State != "succeeded" || item.Phase != "done" || item.Post == nil || item.Post.Exists || item.Post.Side != "remote" {
			t.Fatalf("already-completed delete was not reconciled: %#v", item)
		}
	})

	t.Run("unknown-object-at-path-requires-manual-recovery", func(t *testing.T) {
		_, _, handle := newDeleteJournal(t)
		defer handle.Close()
		sha := testSyncSHA1("new!")
		client := &syncReadOnlyClient{
			dirIDs: map[string]string{"remote": "root"},
			lists:  map[string][]driver.File{"root": {{FileID: "other", Name: "old.bin", Size: 4, Sha1: sha}}},
			files:  map[string]driver.File{"other": {FileID: "other", Name: "old.bin", Size: 4, Sha1: sha}},
		}
		err := handle.reconcileForResume(context.Background(), client)
		if err == nil || !errors.Is(err, errSyncJournalRecoveryRequired) {
			t.Fatalf("ambiguous destructive target was not refused: %v", err)
		}
		if got := handle.snapshot().State; got != "recovery-required" {
			t.Fatalf("ambiguous destructive journal state: got %q", got)
		}
	})
}

func TestSyncJournalResumeReplacementWithWinnerAlreadyPresentMarksCompleted(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "file.bin", "winner")
	info, err := os.Lstat(filepath.Join(localRoot, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	winnerSHA := testSyncSHA1("winner")
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictLocal, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{{
			RelativePath: "file.bin", Action: "replace-remote", Kind: "file", ReplacesKind: "file", Reason: "prefer-local:sha1-mismatch", Destructive: true,
			LocalPresent: true, RemotePresent: true, LocalPath: filepath.Join(localRoot, "file.bin"), RemotePath: "/remote/file.bin",
			LocalSize: int64(len("winner")), RemoteSize: 4, RemoteID: "loser", RemoteSHA1: testSyncSHA1("old!"), LocalModTimeUnixNano: info.ModTime().UnixNano(),
		}},
		DestructiveActions: 1, ChangeActions: 1,
	}
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "winner-started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "winner-id", Name: "file.bin", Size: int64(len("winner")), Sha1: winnerSHA}}},
		files:  map[string]driver.File{"winner-id": {FileID: "winner-id", Name: "file.bin", Size: int64(len("winner")), Sha1: winnerSHA}},
	}
	if err := handle.reconcileForResume(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	item := handle.snapshot().Items[0]
	if item.State != "succeeded" || item.Phase != "done" || item.Post == nil || item.Post.RemoteID != "winner-id" || !strings.EqualFold(item.Post.SHA1, winnerSHA) {
		t.Fatalf("already-created replacement winner was not reconciled: %#v", item)
	}
}

func TestSyncJournalResumeReplacementWithMissingLoserContinuesWinnerOnly(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "file.bin", "winner")
	info, err := os.Lstat(filepath.Join(localRoot, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictLocal, Ready: true, LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{{
			RelativePath: "file.bin", Action: "replace-remote", Kind: "file", ReplacesKind: "file", Reason: "prefer-local:sha1-mismatch", Destructive: true,
			LocalPresent: true, RemotePresent: true, LocalPath: filepath.Join(localRoot, "file.bin"), RemotePath: "/remote/file.bin",
			LocalSize: int64(len("winner")), RemoteSize: 4, RemoteID: "loser", RemoteSHA1: testSyncSHA1("old!"), LocalModTimeUnixNano: info.ModTime().UnixNano(),
		}},
		DestructiveActions: 1, ChangeActions: 1,
	}
	plan.PlanID = syncPlanFingerprint(plan)
	store := testSyncJournalStore(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = "remove-started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client := &syncReadOnlyClient{dirIDs: map[string]string{"remote": "root"}, lists: map[string][]driver.File{"root": {}}, files: map[string]driver.File{}}
	if err := handle.reconcileForResume(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	snapshot := handle.snapshot()
	if snapshot.Items[0].State != "pending" || snapshot.Items[0].Phase != "loser-removed" || snapshot.State != "active" {
		t.Fatalf("missing loser was not converted into winner-only resume: %#v", snapshot.Items[0])
	}
	residual := buildSyncJournalResidualPlan(snapshot)
	if residual.Items[0].Action != "upload" || residual.Items[0].Destructive || residual.DestructiveActions != 0 || residual.Items[0].Reason != "journal-resume-winner-only:replace-remote" {
		t.Fatalf("winner-only residual plan is unsafe: %#v", residual)
	}
	if err := preflightSyncJournalResume(context.Background(), client, snapshot); err != nil {
		t.Fatalf("winner-only resume preflight rejected proven missing loser: %v", err)
	}
}

func TestPreflightSyncJournalResumeAcceptsMixedCompletedAndPendingTree(t *testing.T) {
	localRoot := t.TempDir()
	writeSyncTestFile(t, localRoot, "dir/pending.bin", "child")
	client := &syncReadOnlyClient{dirIDs: map[string]string{"remote": "root"}, lists: map[string][]driver.File{"root": {}}, files: map[string]driver.File{}}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionUpload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("c", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	dirIndex := -1
	for index, item := range plan.Items {
		if item.RelativePath == "dir" {
			dirIndex = index
			break
		}
	}
	if dirIndex < 0 {
		t.Fatalf("directory action missing from plan: %#v", plan.Items)
	}
	journal.Items[dirIndex].State = "succeeded"
	journal.Items[dirIndex].Phase = "done"
	journal.Items[dirIndex].Post = &syncJournalPostcondition{Side: "remote", Exists: true, Kind: "directory", RemoteID: "remote-dir"}
	client.lists["root"] = []driver.File{{FileID: "remote-dir", Name: "dir", IsDirectory: true}}
	client.lists["remote-dir"] = nil
	if err := preflightSyncJournalResume(context.Background(), client, journal); err != nil {
		t.Fatalf("valid mixed resume tree failed preflight: %v", err)
	}
	client.lists["remote-dir"] = []driver.File{{FileID: "surprise", Name: "surprise.bin", Size: 1, Sha1: testSyncSHA1("x")}}
	if err := preflightSyncJournalResume(context.Background(), client, journal); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("unexpected remote entry was not rejected during mixed resume preflight: %v", err)
	}
}

func TestPreflightSyncJournalResumeIgnoresOnlyKnownInterruptedDownloadArtifacts(t *testing.T) {
	localRoot := t.TempDir()
	sha := testSyncSHA1("data")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}}},
		files:  map[string]driver.File{"remote-file": {FileID: "remote-file", Name: "file.bin", Size: 4, Sha1: sha}},
	}
	plan, err := buildSyncPlanWithOptions(client, localRoot, "/remote", syncPlanOptions{Direction: syncDirectionDownload, ConflictPolicy: syncConflictError})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("d", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Items) != 1 || journal.Plan.Items[0].Action != "download" {
		t.Fatalf("unexpected download plan: %#v", journal.Plan.Items)
	}
	journal.Items[0].State = "failed"
	journal.Items[0].Phase = "mutation-started"
	writeSyncTestFile(t, localRoot, ".file.bin.115driver.part", "part")
	if err := preflightSyncJournalResume(context.Background(), client, journal); err != nil {
		t.Fatalf("known interrupted download artifact failed resume preflight: %v", err)
	}
	writeSyncTestFile(t, localRoot, "unrelated.tmp", "x")
	if err := preflightSyncJournalResume(context.Background(), client, journal); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Fatalf("unrelated local artifact was incorrectly ignored: %v", err)
	}
}

func TestCaptureSyncRemotePostconditionRejectsDuplicateNames(t *testing.T) {
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists: map[string][]driver.File{"root": {
			{FileID: "first", Name: "same.bin", Size: 1},
			{FileID: "second", Name: "same.bin", Size: 1},
		}},
		files: map[string]driver.File{},
	}
	if _, _, err := captureSyncRemotePostcondition(client, "/remote/same.bin"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate remote postcondition target was accepted: %v", err)
	}
	if client.getFileCalls != 0 {
		t.Fatalf("ambiguous target should fail before choosing a file ID; GetFile calls=%d", client.getFileCalls)
	}
}

func TestValidateSyncResumeRootsRequiresOriginalRoots(t *testing.T) {
	plan := testSyncJournalPlan(t)
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("e", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSyncResumeRoots(journal, plan.LocalRoot, "/remote/"); err != nil {
		t.Fatalf("equivalent resume roots were rejected: %v", err)
	}
	if err := validateSyncResumeRoots(journal, filepath.Join(plan.LocalRoot, "other"), "/remote"); err == nil || !strings.Contains(err.Error(), "local root mismatch") {
		t.Fatalf("different local resume root was accepted: %v", err)
	}
	if err := validateSyncResumeRoots(journal, plan.LocalRoot, "/elsewhere"); err == nil || !strings.Contains(err.Error(), "remote root mismatch") {
		t.Fatalf("different remote resume root was accepted: %v", err)
	}
}

func newSyncResumeFlagTestCommand(t *testing.T, changed string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("direction", syncDirectionBoth, "")
	cmd.Flags().String("conflict", syncConflictError, "")
	cmd.Flags().Bool("delete", false, "")
	if changed != "" {
		values := map[string]string{"direction": "upload", "conflict": "prefer-local", "delete": "true"}
		if err := cmd.Flags().Set(changed, values[changed]); err != nil {
			t.Fatal(err)
		}
	}
	return cmd
}

func TestValidateSyncResumeFlagContractFailsClosed(t *testing.T) {
	oldDryRun, oldCheck, oldNoJournal := syncDryRun, syncCheck, syncNoJournal
	defer func() { syncDryRun, syncCheck, syncNoJournal = oldDryRun, oldCheck, oldNoJournal }()
	syncDryRun, syncCheck, syncNoJournal = false, false, false
	if err := validateSyncResumeFlagContract(newSyncResumeFlagTestCommand(t, ""), "abcdef12"); err != nil {
		t.Fatalf("basic resume contract rejected: %v", err)
	}
	for _, changed := range []string{"direction", "conflict", "delete"} {
		if err := validateSyncResumeFlagContract(newSyncResumeFlagTestCommand(t, changed), "abcdef12"); err == nil || !strings.Contains(err.Error(), "stored plan policy") {
			t.Fatalf("resume accepted changed --%s: %v", changed, err)
		}
	}
	syncNoJournal = true
	if err := validateSyncResumeFlagContract(newSyncResumeFlagTestCommand(t, ""), "abcdef12"); err == nil || !strings.Contains(err.Error(), "--no-journal") {
		t.Fatalf("resume accepted --no-journal: %v", err)
	}
	syncNoJournal = false
	syncCheck = true
	if err := validateSyncResumeFlagContract(newSyncResumeFlagTestCommand(t, ""), "abcdef12"); err == nil || !strings.Contains(err.Error(), "--check") {
		t.Fatalf("resume accepted read-only mode: %v", err)
	}
}

func TestSyncJournalAdminCommandsAreRegisteredAndAuthIndependent(t *testing.T) {
	want := map[string]bool{"schema": false, "list": false, "inspect": false, "doctor": false, "verify": false, "recover": false, "migrate": false, "rm": false, "gc": false, "trash": false, "aliases": false}
	for _, command := range syncJournalCmd.Commands() {
		if _, ok := want[command.Name()]; ok {
			want[command.Name()] = true
		}
	}
	trashChildren := map[string]bool{"list": false, "restore": false}
	for _, command := range syncJournalTrashCmd.Commands() {
		if _, ok := trashChildren[command.Name()]; ok {
			trashChildren[command.Name()] = true
		}
	}
	for name, found := range trashChildren {
		if !found {
			t.Fatalf("sync journal trash command %q is not registered", name)
		}
	}
	aliasChildren := map[string]bool{"diagnose": false, "plan": false, "reconcile": false, "reconcile-batch": false}
	for _, command := range syncJournalAliasesCmd.Commands() {
		if _, ok := aliasChildren[command.Name()]; ok {
			aliasChildren[command.Name()] = true
		}
	}
	for name, found := range aliasChildren {
		if !found {
			t.Fatalf("sync journal aliases command %q is not registered", name)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("sync journal command %q is not registered", name)
		}
	}
	force := syncJournalRmCmd.Flags().Lookup("force")
	if force == nil || force.DefValue != "false" {
		t.Fatalf("sync journal rm --force default: %#v", force)
	}
	migrateAll := syncJournalMigrateCmd.Flags().Lookup("all")
	if migrateAll == nil || migrateAll.DefValue != "false" {
		t.Fatalf("sync journal migrate --all default: %#v", migrateAll)
	}
	recoverBatch := syncJournalMigrateCmd.Flags().Lookup("recover-batch")
	if recoverBatch == nil || recoverBatch.DefValue != "false" {
		t.Fatalf("sync journal migrate --recover-batch default: %#v", recoverBatch)
	}
	repairID := syncJournalAliasesReconcileCmd.Flags().Lookup("expect-repair-id")
	if repairID == nil || repairID.DefValue != "" {
		t.Fatalf("sync journal aliases reconcile --expect-repair-id default: %#v", repairID)
	}
	repairSetID := syncJournalAliasesReconcileBatchCmd.Flags().Lookup("expect-repair-set-id")
	if repairSetID == nil || repairSetID.DefValue != "" {
		t.Fatalf("sync journal aliases reconcile-batch --expect-repair-set-id default: %#v", repairSetID)
	}
	planLimit := syncJournalAliasesPlanCmd.Flags().Lookup("limit")
	batchLimit := syncJournalAliasesReconcileBatchCmd.Flags().Lookup("limit")
	if planLimit == nil || planLimit.DefValue != "50" || batchLimit == nil || batchLimit.DefValue != "50" {
		t.Fatalf("sync journal aliases batch --limit defaults: plan=%#v batch=%#v", planLimit, batchLimit)
	}
	for _, command := range []*cobra.Command{syncJournalSchemaCmd, syncJournalListCmd, syncJournalInspectCmd, syncJournalDoctorCmd, syncJournalMigrateCmd, syncJournalRmCmd, syncJournalGCCmd, syncJournalTrashCmd, syncJournalTrashListCmd, syncJournalTrashRestoreCmd, syncJournalAliasesCmd, syncJournalAliasesDiagnoseCmd, syncJournalAliasesPlanCmd, syncJournalAliasesReconcileCmd, syncJournalAliasesReconcileBatchCmd} {
		if !commandSkipsAuthentication(command) {
			t.Fatalf("sync journal %s unexpectedly requires authentication", command.Name())
		}
	}
	for _, command := range []*cobra.Command{syncJournalVerifyCmd, syncJournalRecoverCmd} {
		if commandSkipsAuthentication(command) {
			t.Fatalf("sync journal %s must authenticate before reading live remote state", command.Name())
		}
	}
	if err := rootCmd.PersistentPreRunE(syncJournalListCmd, nil); err != nil {
		t.Fatalf("sync journal admin unexpectedly required authentication: %v", err)
	}
}
