package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

func testSyncDeleteRemoteJournal(t *testing.T, remoteID, contents string) syncExecutionJournal {
	t.Helper()
	localRoot := t.TempDir()
	sha := testSyncSHA1(contents)
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: syncDirectionUpload,
		ConflictPolicy: syncConflictError, DeleteExtraneous: true, Ready: true,
		LocalRoot: localRoot, RemoteRoot: "/remote", RemoteRootID: "root",
		Items: []syncPlanItem{{
			RelativePath: "old.bin", Action: "delete-remote", Kind: "file", Reason: "mirror-delete:remote-only",
			LocalPresent: false, RemotePresent: true, LocalPath: localRoot + "/old.bin", RemotePath: "/remote/old.bin",
			RemoteID: remoteID, RemoteSize: int64(len(contents)), RemoteSHA1: sha, Destructive: true,
		}},
		DestructiveActions: 1, ChangeActions: 1,
	}
	plan.PlanID = syncPlanFingerprint(plan)
	journal, err := newSyncExecutionJournal(plan, strings.Repeat("a", 64), 42)
	if err != nil {
		t.Fatal(err)
	}
	journal.State = "failed"
	journal.Items[0].State = "failed"
	journal.Items[0].Phase = "delete-started"
	return journal
}

func TestVerifySyncJournalResumeClassifiesSafeFullRetryWithoutMutation(t *testing.T) {
	journal := testSyncDeleteRemoteJournal(t, "old", "old!")
	journal.Version = 1
	before := cloneSyncExecutionJournal(journal)
	sha := testSyncSHA1("old!")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "old", Name: "old.bin", Size: 4, Sha1: sha}}},
		files:  map[string]driver.File{"old": {FileID: "old", Name: "old.bin", Size: 4, Sha1: sha}},
	}
	result := verifySyncJournalResume(context.Background(), client, journal)
	if result.Schema != syncJournalVerificationSchema || !result.ResumeReady || !result.PreflightPassed || result.RetryFull != 1 || result.Errors != 0 || result.JournalVersion != 1 || !result.MigrationRequired {
		t.Fatalf("safe destructive full retry was not verified with legacy schema metadata: %#v", result)
	}
	if len(result.Items) != 1 || result.Items[0].Decision != syncJournalDestructiveRetryFull {
		t.Fatalf("unexpected verification decision: %#v", result.Items)
	}
	if journal.State != before.State || journal.Items[0].State != before.Items[0].State || journal.Items[0].Phase != before.Items[0].Phase {
		t.Fatalf("read-only verify mutated caller journal: before=%#v after=%#v", before.Items[0], journal.Items[0])
	}
}

func TestVerifySyncJournalResumeReportsAmbiguousDestructiveTarget(t *testing.T) {
	journal := testSyncDeleteRemoteJournal(t, "old", "old!")
	sha := testSyncSHA1("new!")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {{FileID: "other", Name: "old.bin", Size: 4, Sha1: sha}}},
		files:  map[string]driver.File{"other": {FileID: "other", Name: "old.bin", Size: 4, Sha1: sha}},
	}
	result := verifySyncJournalResume(context.Background(), client, journal)
	if result.ResumeReady || result.PreflightPassed || !result.RecoveryRequired || result.Errors != 1 {
		t.Fatalf("ambiguous destructive target was not surfaced: %#v", result)
	}
	if len(result.Items) != 1 || result.Items[0].Decision != syncJournalDestructiveAmbiguous || result.Items[0].Error == "" {
		t.Fatalf("ambiguous verification details missing: %#v", result.Items)
	}
}

func TestVerifySyncJournalResumeDetectsAlreadyCompletedDelete(t *testing.T) {
	journal := testSyncDeleteRemoteJournal(t, "old", "old!")
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	result := verifySyncJournalResume(context.Background(), client, journal)
	if !result.ResumeReady || !result.PreflightPassed || result.CompletedDetected != 1 || result.Errors != 0 {
		t.Fatalf("already-completed delete was not verified: %#v", result)
	}
	if len(result.Items) != 1 || result.Items[0].Decision != syncJournalDestructiveCompleted {
		t.Fatalf("completed delete verification decision: %#v", result.Items)
	}
}

func TestVerifyRecoveryRequiredJournalCanBeClearableWithoutBeingResumeReady(t *testing.T) {
	journal := testSyncDeleteRemoteJournal(t, "old", "old!")
	journal.State = "recovery-required"
	client := &syncReadOnlyClient{
		dirIDs: map[string]string{"remote": "root"},
		lists:  map[string][]driver.File{"root": {}},
		files:  map[string]driver.File{},
	}
	result := verifySyncJournalResume(context.Background(), client, journal)
	if result.ResumeReady || !result.RecoveryRequired || !result.RecoveryClearable || !result.PreflightPassed || result.Errors != 0 {
		t.Fatalf("reviewable recovery latch was misclassified: %#v", result)
	}
}

func TestReconcileSyncJournalAfterReviewClearsLatchOnlyAfterPreflight(t *testing.T) {
	newHandle := func(t *testing.T) (*syncJournalHandle, *syncReadOnlyClient) {
		t.Helper()
		journal := testSyncDeleteRemoteJournal(t, "old", "old!")
		store := testSyncJournalStore(t)
		handle, err := store.Create(journal.Plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.mutate(func(stored *syncExecutionJournal) error {
			stored.State = "recovery-required"
			stored.LastError = "manual review required"
			stored.Items[0].State = "failed"
			stored.Items[0].Phase = "delete-started"
			return nil
		}); err != nil {
			handle.Close()
			t.Fatal(err)
		}
		client := &syncReadOnlyClient{
			dirIDs: map[string]string{"remote": "root"},
			lists:  map[string][]driver.File{"root": {}},
			files:  map[string]driver.File{},
		}
		return handle, client
	}

	t.Run("safe-evidence-clears-latch", func(t *testing.T) {
		handle, client := newHandle(t)
		defer handle.Close()
		if err := handle.reconcileForResumeAfterReview(context.Background(), client); err != nil {
			t.Fatal(err)
		}
		snapshot := handle.snapshot()
		if snapshot.State != "active" || snapshot.Items[0].State != "succeeded" || snapshot.Items[0].Phase != "done" || snapshot.Items[0].Post == nil || snapshot.Items[0].Post.Exists {
			t.Fatalf("safe reviewed recovery was not persisted: %#v", snapshot)
		}
	})

	t.Run("tree-drift-preserves-recovery-latch", func(t *testing.T) {
		handle, client := newHandle(t)
		defer handle.Close()
		writeSyncTestFile(t, handle.snapshot().Plan.LocalRoot, "surprise.bin", "x")
		err := handle.reconcileForResumeAfterReview(context.Background(), client)
		if err == nil || !strings.Contains(err.Error(), "preflight") {
			t.Fatalf("reviewed recovery ignored whole-tree drift: %v", err)
		}
		snapshot := handle.snapshot()
		if snapshot.State != "recovery-required" || snapshot.Items[0].State != "failed" || snapshot.Items[0].Phase != "delete-started" {
			t.Fatalf("failed reviewed preflight cleared recovery latch: %#v", snapshot)
		}
	})
}
