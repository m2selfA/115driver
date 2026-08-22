package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

func createSessionForRemoveTest(t *testing.T, store transfer.SessionStore, name string) string {
	t.Helper()
	identity, err := transfer.NewSessionIdentityV2(
		"upload",
		"file",
		"test-scope",
		filepath.Join(t.TempDir(), name),
		"/"+name,
		"multipart",
		"single-file",
	)
	if err != nil {
		t.Fatal(err)
	}
	location, _, err := store.Open(identity, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return location.ID
}

func TestPrepareSessionRemovePlansResolvesAllInputsAndRejectsDuplicates(t *testing.T) {
	store := transfer.SessionStore{Root: t.TempDir()}
	first := createSessionForRemoveTest(t, store, "first.bin")
	second := createSessionForRemoveTest(t, store, "second.bin")

	plans := prepareSessionRemovePlans(store, []string{first, first, "missing-session", second})
	if len(plans) != 4 {
		t.Fatalf("unexpected plans: %#v", plans)
	}
	if plans[0].Err != nil || plans[0].ID != first {
		t.Fatalf("first session did not resolve: %#v", plans[0])
	}
	if plans[1].Err == nil || commandErrorCode(plans[1].Err) != output.ExitArgs {
		t.Fatalf("duplicate session was not rejected as an argument error: %#v", plans[1])
	}
	if plans[2].Err == nil || commandErrorCode(plans[2].Err) != output.ExitNotFound {
		t.Fatalf("missing session used wrong error: %#v", plans[2])
	}
	if plans[3].Err != nil || plans[3].ID != second {
		t.Fatalf("second session did not resolve: %#v", plans[3])
	}
}

func TestRemoveManagedSessionWithoutRemoteAbortMovesOnlyRequestedSession(t *testing.T) {
	store := transfer.SessionStore{Root: t.TempDir()}
	first := createSessionForRemoveTest(t, store, "first.bin")
	second := createSessionForRemoveTest(t, store, "second.bin")

	result, err := removeManagedSession(context.Background(), store, first, false, uploadpkg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != first || result.TrashPath == "" || result.RemoteMultipartAborted != 0 {
		t.Fatalf("unexpected remove result: %#v", result)
	}
	if info, err := os.Stat(result.TrashPath); err != nil || !info.IsDir() {
		t.Fatalf("trash path missing: info=%v err=%v", info, err)
	}
	if _, err := store.InspectSession(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed session still inspectable: %v", err)
	}
	if entry, err := store.InspectSession(second); err != nil || entry.ID != second {
		t.Fatalf("unrelated session was affected: entry=%#v err=%v", entry, err)
	}
}
