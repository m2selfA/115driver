package cmd

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type offlineDeleteCall struct {
	hashes      []string
	deleteFiles bool
}

type fakeOfflineRemoveClient struct {
	tasks   []*driver.OfflineTask
	deletes []offlineDeleteCall
	err     error
}

func (f *fakeOfflineRemoveClient) ListOfflineTask(page int64) (driver.OfflineTaskResp, error) {
	if f.err != nil {
		return driver.OfflineTaskResp{}, f.err
	}
	if page != 1 {
		return driver.OfflineTaskResp{Page: page, PageCount: 1}, nil
	}
	return driver.OfflineTaskResp{Page: 1, PageCount: 1, Total: int64(len(f.tasks)), Tasks: f.tasks}, nil
}

func (f *fakeOfflineRemoveClient) DeleteOfflineTasks(hashes []string, deleteFiles bool) error {
	f.deletes = append(f.deletes, offlineDeleteCall{hashes: append([]string(nil), hashes...), deleteFiles: deleteFiles})
	return f.err
}

func preserveOfflineRmGlobals(t *testing.T) {
	t.Helper()
	oldDryRun, oldDeleteFiles, oldForce := offlineRmDryRun, offlineRmDeleteFiles, offlineRmForce
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		offlineRmDryRun, offlineRmDeleteFiles, offlineRmForce = oldDryRun, oldDeleteFiles, oldForce
		jsonOutput, printer = oldJSON, oldPrinter
	})
}

func newOfflineRmTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "rm"}
	addBatchFromFileFlag(cmd)
	addContinueOnErrorFlag(cmd)
	return cmd
}

func TestOfflineRmDeleteFilesRequiresForceBeforeExecution(t *testing.T) {
	preserveOfflineRmGlobals(t)
	cmd := newOfflineRmTestCommand(t)
	offlineRmDeleteFiles, offlineRmDryRun, offlineRmForce = true, false, false
	err := offlineRmArgs(cmd, []string{"hash-1"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs {
		t.Fatalf("delete-files without force = %T %v, want ExitArgs", err, err)
	}

	offlineRmDryRun = true
	if err := offlineRmArgs(cmd, []string{"hash-1"}); err != nil {
		t.Fatalf("delete-files dry-run unexpectedly requires force: %v", err)
	}
	offlineRmDryRun, offlineRmForce = false, true
	if err := offlineRmArgs(cmd, []string{"hash-1"}); err != nil {
		t.Fatalf("forced delete-files rejected: %v", err)
	}
	offlineRmDeleteFiles, offlineRmForce = false, false
	if err := offlineRmArgs(cmd, []string{"hash-1"}); err != nil {
		t.Fatalf("ordinary offline rm behavior changed: %v", err)
	}
}

func TestOfflineRmDeleteFilesDryRunShowsAssociatedIDsWithoutMutation(t *testing.T) {
	client := &fakeOfflineRemoveClient{tasks: []*driver.OfflineTask{{
		InfoHash: "ABC", Name: "task", Status: 2, FileId: "file-1", DelFileId: "delete-file-1",
	}}}
	plan, err := buildOfflineRemovePlan(client, []string{"abc"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DeleteFiles || len(plan.Items) != 1 || plan.Items[0].FileID != "file-1" || plan.Items[0].DeleteFileID != "delete-file-1" {
		t.Fatalf("unexpected delete-files dry-run plan: %#v", plan)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("dry-run plan mutated remote state: %#v", client.deletes)
	}
}

func TestRunOfflineRemovePassesDeleteFilesToDriver(t *testing.T) {
	preserveOfflineRmGlobals(t)
	offlineRmDeleteFiles, offlineRmDryRun, offlineRmForce = true, false, true
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeOfflineRemoveClient{}
	cmd := newOfflineRmTestCommand(t)
	if err := runOfflineRemove(client, cmd, []string{"hash-1"}); err != nil {
		t.Fatal(err)
	}
	if len(client.deletes) != 1 || !client.deletes[0].deleteFiles || len(client.deletes[0].hashes) != 1 || client.deletes[0].hashes[0] != "hash-1" {
		t.Fatalf("unexpected delete call: %#v", client.deletes)
	}
}

func TestRunOfflineRemoveContinueOnErrorPreservesDeleteFiles(t *testing.T) {
	preserveOfflineRmGlobals(t)
	offlineRmDeleteFiles, offlineRmDryRun, offlineRmForce = true, false, true
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeOfflineRemoveClient{}
	cmd := newOfflineRmTestCommand(t)
	if err := cmd.Flags().Set("continue-on-error", "true"); err != nil {
		t.Fatal(err)
	}
	if err := runOfflineRemove(client, cmd, []string{"hash-1", "hash-2"}); err != nil {
		t.Fatal(err)
	}
	if len(client.deletes) != 2 {
		t.Fatalf("delete calls = %#v, want two item calls", client.deletes)
	}
	for _, call := range client.deletes {
		if !call.deleteFiles {
			t.Fatalf("continue-on-error lost delete-files policy: %#v", client.deletes)
		}
	}
}
