package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func TestDURejectsNegativeDepthDuringArgumentValidation(t *testing.T) {
	oldDepth := duMaxDepth
	t.Cleanup(func() { duMaxDepth = oldDepth })
	duMaxDepth = -1

	err := duInputArgs(duCmd, []string{"/remote"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "--max-depth must be >= 0") {
		t.Fatalf("unexpected du max-depth result: %T %v", err, err)
	}
}

func TestRmStdinConfirmationConflictFailsDuringArgumentValidation(t *testing.T) {
	oldForce, oldDryRun := rmForce, rmDryRun
	t.Cleanup(func() { rmForce, rmDryRun = oldForce, oldDryRun })
	rmForce = false
	rmDryRun = false

	cmd := newBatchInputTestCommand(t, "-")
	err := rmInputArgs(cmd, nil)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("unexpected rm stdin safety result: %T %v", err, err)
	}
}

func TestRmStdinDryRunDoesNotRequireForce(t *testing.T) {
	oldForce, oldDryRun := rmForce, rmDryRun
	t.Cleanup(func() { rmForce, rmDryRun = oldForce, oldDryRun })
	rmForce = false
	rmDryRun = true

	cmd := newBatchInputTestCommand(t, "-")
	if err := rmInputArgs(cmd, nil); err != nil {
		t.Fatalf("rm dry-run stdin was rejected: %v", err)
	}
}

func newTransferPreauthTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := newBatchInputTestCommand(t, "")
	cmd.Flags().Int("workers-per-interface", 0, "")
	return cmd
}

func requireExitArgs(t *testing.T, err error) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs {
		t.Fatalf("error = %T %v, want ExitArgs", err, err)
	}
}

func TestDownloadPureFlagValidationRunsInArgs(t *testing.T) {
	oldTimeout, oldWorkers := downloadTimeout, downloadWorkersPerInterface
	oldStrategy, oldChunk, oldSession := downloadStrategy, downloadChunkSize, downloadSession
	oldRecursive := downloadRecursive
	t.Cleanup(func() {
		downloadTimeout, downloadWorkersPerInterface = oldTimeout, oldWorkers
		downloadStrategy, downloadChunkSize, downloadSession = oldStrategy, oldChunk, oldSession
		downloadRecursive = oldRecursive
	})

	reset := func() *cobra.Command {
		downloadTimeout = defaultDownloadTimeout
		downloadWorkersPerInterface = 0
		downloadStrategy = ""
		downloadChunkSize = ""
		downloadSession = ""
		downloadRecursive = false
		return newTransferPreauthTestCommand(t)
	}

	cmd := reset()
	if err := cmd.Flags().Set("jobs", "0"); err != nil {
		t.Fatal(err)
	}
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))

	cmd = reset()
	downloadTimeout = -1
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))

	cmd = reset()
	downloadWorkersPerInterface = 0
	if err := cmd.Flags().Set("workers-per-interface", "0"); err != nil {
		t.Fatal(err)
	}
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))

	cmd = reset()
	downloadStrategy = "invalid"
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))

	cmd = reset()
	downloadChunkSize = "not-a-size"
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))

	cmd = reset()
	downloadSession = "session.json"
	requireExitArgs(t, downloadInputArgs(cmd, []string{"/remote", "./local"}))
}

func TestUploadPureFlagValidationRunsInArgs(t *testing.T) {
	oldTimeout, oldWorkers := uploadTimeout, uploadWorkersPerInterface
	oldChunk, oldContents, oldRecursive := uploadChunkSize, uploadContents, uploadRecursive
	t.Cleanup(func() {
		uploadTimeout, uploadWorkersPerInterface = oldTimeout, oldWorkers
		uploadChunkSize, uploadContents, uploadRecursive = oldChunk, oldContents, oldRecursive
	})

	reset := func() *cobra.Command {
		uploadTimeout = 0
		uploadWorkersPerInterface = 0
		uploadChunkSize = ""
		uploadContents = false
		uploadRecursive = false
		return newTransferPreauthTestCommand(t)
	}

	cmd := reset()
	if err := cmd.Flags().Set("jobs", "0"); err != nil {
		t.Fatal(err)
	}
	requireExitArgs(t, uploadInputArgs(cmd, []string{"local.bin", "/remote"}))

	cmd = reset()
	uploadTimeout = -1
	requireExitArgs(t, uploadInputArgs(cmd, []string{"local.bin", "/remote"}))

	cmd = reset()
	uploadWorkersPerInterface = 0
	if err := cmd.Flags().Set("workers-per-interface", "0"); err != nil {
		t.Fatal(err)
	}
	requireExitArgs(t, uploadInputArgs(cmd, []string{"local.bin", "/remote"}))

	cmd = reset()
	uploadChunkSize = "1KiB"
	requireExitArgs(t, uploadInputArgs(cmd, []string{"local.bin", "/remote"}))

	cmd = reset()
	uploadContents = true
	requireExitArgs(t, uploadInputArgs(cmd, []string{"local.bin", "/remote"}))
}

func TestSyncPureFlagValidationRunsInArgs(t *testing.T) {
	oldDryRun, oldCheck := syncDryRun, syncCheck
	oldContinue, oldDelete := syncContinueOnError, syncDelete
	oldNoJournal := syncNoJournal
	oldMaxErrors, oldMaxRoots, oldMaxItems, oldJobs := syncMaxErrors, syncMaxDeleteRoots, syncMaxDeleteItems, syncJobs
	oldExpect, oldResume, oldMaxBytes := syncExpectPlan, syncResume, syncMaxDeleteBytes
	oldDirection, oldConflict := syncDirection, syncConflictPolicy
	t.Cleanup(func() {
		syncDryRun, syncCheck = oldDryRun, oldCheck
		syncContinueOnError, syncDelete, syncNoJournal = oldContinue, oldDelete, oldNoJournal
		syncMaxErrors, syncMaxDeleteRoots, syncMaxDeleteItems, syncJobs = oldMaxErrors, oldMaxRoots, oldMaxItems, oldJobs
		syncExpectPlan, syncResume, syncMaxDeleteBytes = oldExpect, oldResume, oldMaxBytes
		syncDirection, syncConflictPolicy = oldDirection, oldConflict
	})

	reset := func() {
		syncDryRun = false
		syncCheck = false
		syncContinueOnError = false
		syncDelete = false
		syncNoJournal = false
		syncMaxErrors = 0
		syncMaxDeleteRoots = 0
		syncMaxDeleteItems = 0
		syncJobs = 1
		syncExpectPlan = ""
		syncResume = ""
		syncMaxDeleteBytes = ""
		syncDirection = syncDirectionBoth
		syncConflictPolicy = syncConflictError
	}
	args := []string{"./local", "/remote"}

	for name, configure := range map[string]func(){
		"jobs":             func() { syncJobs = 0 },
		"max-errors":       func() { syncMaxErrors = 1 },
		"expect-plan":      func() { syncExpectPlan = "not-a-plan-id" },
		"max-delete-roots": func() { syncMaxDeleteRoots = -1 },
		"max-delete-items": func() { syncMaxDeleteItems = -1 },
		"max-delete-bytes": func() { syncMaxDeleteBytes = "not-a-size" },
		"direction":        func() { syncDirection = "sideways" },
		"conflict":         func() { syncConflictPolicy = "latest-wins" },
		"delete-direction": func() { syncDelete = true },
		"budget-no-delete": func() { syncMaxDeleteRoots = 1 },
		"resume-dry-run":   func() { syncResume = "abc"; syncDryRun = true },
	} {
		t.Run(name, func(t *testing.T) {
			reset()
			configure()
			requireExitArgs(t, syncInputArgs(syncCmd, args))
		})
	}
}
