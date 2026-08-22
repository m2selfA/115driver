package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func TestRecursiveAndSessionFlagsHaveShortForms(t *testing.T) {
	tests := []struct {
		name      string
		flagName  string
		shorthand string
		lookup    func(string) string
	}{
		{name: "upload recursive", flagName: "recursive", shorthand: "r", lookup: func(name string) string { return uploadCmd.Flags().Lookup(name).Shorthand }},
		{name: "download recursive", flagName: "recursive", shorthand: "r", lookup: func(name string) string { return downloadCmd.Flags().Lookup(name).Shorthand }},
		{name: "ls recursive", flagName: "recursive", shorthand: "R", lookup: func(name string) string { return lsCmd.Flags().Lookup(name).Shorthand }},
		{name: "upload session", flagName: "session", shorthand: "s", lookup: func(name string) string { return uploadCmd.Flags().Lookup(name).Shorthand }},
		{name: "download session", flagName: "session", shorthand: "s", lookup: func(name string) string { return downloadCmd.Flags().Lookup(name).Shorthand }},
		{name: "search directory", flagName: "dir", shorthand: "d", lookup: func(name string) string { return searchCmd.Flags().Lookup(name).Shorthand }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.lookup(test.flagName); got != test.shorthand {
				t.Fatalf("flag --%s shorthand: got %q want %q", test.flagName, got, test.shorthand)
			}
		})
	}
}

func TestUploadExposesContentsMode(t *testing.T) {
	flag := uploadCmd.Flags().Lookup("contents")
	if flag == nil {
		t.Fatal("upload command is missing --contents")
	}
	if flag.Shorthand != "" {
		t.Fatalf("upload --contents unexpectedly has shorthand %q", flag.Shorthand)
	}
	if !strings.Contains(uploadCmd.Long, "copied by name") || !strings.Contains(uploadCmd.Example, "--contents") {
		t.Fatalf("upload help does not explain recursive directory semantics: long=%q example=%q", uploadCmd.Long, uploadCmd.Example)
	}
}

func TestTransferCommandsExposeWorkersPerInterfaceOverride(t *testing.T) {
	for name, command := range map[string]*cobra.Command{"upload": uploadCmd, "download": downloadCmd} {
		flag := command.Flags().Lookup("workers-per-interface")
		if flag == nil {
			t.Fatalf("%s command is missing --workers-per-interface", name)
		}
		if flag.Shorthand != "" {
			t.Fatalf("%s --workers-per-interface unexpectedly has shorthand %q", name, flag.Shorthand)
		}
	}
}

func TestBatchRemoteOperationCommandsAcceptMultipleSources(t *testing.T) {
	if err := cpCmd.Args(cpCmd, []string{"a", "b", "/dest"}); err != nil {
		t.Fatalf("cp rejected multiple sources: %v", err)
	}
	if err := mvCmd.Args(mvCmd, []string{"a", "b", "/dest"}); err != nil {
		t.Fatalf("mv rejected multiple sources: %v", err)
	}
	if err := rmCmd.Args(rmCmd, []string{"a", "b"}); err != nil {
		t.Fatalf("rm rejected multiple paths: %v", err)
	}
	if err := cpCmd.Args(cpCmd, []string{"only-one"}); err == nil {
		t.Fatal("cp accepted missing destination")
	}
}

func TestTransferAndOfflineCommandsAcceptBatchArguments(t *testing.T) {
	for name, command := range map[string]*cobra.Command{
		"upload":      uploadCmd,
		"download":    downloadCmd,
		"offline add": offlineAddCmd,
		"offline rm":  offlineRmCmd,
	} {
		args := []string{"a", "b"}
		if name == "upload" || name == "download" {
			args = []string{"a", "b", "destination"}
		}
		if err := command.Args(command, args); err != nil {
			t.Fatalf("%s rejected batch arguments: %v", name, err)
		}
	}
	if err := uploadCmd.Args(uploadCmd, []string{"only-one"}); err == nil {
		t.Fatal("upload accepted missing destination")
	}
	if err := downloadCmd.Args(downloadCmd, []string{"only-one"}); err == nil {
		t.Fatal("download accepted missing destination")
	}
	if !strings.Contains(uploadCmd.Use, "...") || !strings.Contains(downloadCmd.Use, "...") {
		t.Fatalf("transfer help does not advertise multiple sources: upload=%q download=%q", uploadCmd.Use, downloadCmd.Use)
	}
}

func TestMkdirAcceptsMultiplePaths(t *testing.T) {
	if err := mkdirCmd.Args(mkdirCmd, []string{"/a", "/b", "/c"}); err != nil {
		t.Fatalf("mkdir rejected multiple paths: %v", err)
	}
	if err := mkdirCmd.Args(mkdirCmd, nil); err == nil {
		t.Fatal("mkdir accepted an empty path list")
	}
	if !strings.Contains(mkdirCmd.Use, "...") {
		t.Fatalf("mkdir help does not advertise multiple paths: %q", mkdirCmd.Use)
	}
}

func TestReadOnlyMetadataCommandsAcceptMultiplePaths(t *testing.T) {
	for name, command := range map[string]*cobra.Command{"stat": statCmd, "du": duCmd} {
		if err := command.Args(command, []string{"/a", "/b"}); err != nil {
			t.Fatalf("%s rejected multiple paths: %v", name, err)
		}
		if err := command.Args(command, nil); err == nil {
			t.Fatalf("%s accepted an empty path list", name)
		}
		if !strings.Contains(command.Use, "...") {
			t.Fatalf("%s help does not advertise multiple paths: %q", name, command.Use)
		}
	}
}

func TestLSAcceptsDefaultRootAndMultiplePaths(t *testing.T) {
	if err := lsCmd.Args(lsCmd, nil); err != nil {
		t.Fatalf("ls rejected default-root invocation: %v", err)
	}
	if err := lsCmd.Args(lsCmd, []string{"/a", "/b"}); err != nil {
		t.Fatalf("ls rejected multiple paths: %v", err)
	}
	if !strings.Contains(lsCmd.Use, "...") {
		t.Fatalf("ls help does not advertise multiple paths: %q", lsCmd.Use)
	}
}

func TestLSRejectsInvalidDepthDuringArgumentValidation(t *testing.T) {
	oldRecursive, oldDepth := lsRecursive, lsMaxDepth
	t.Cleanup(func() { lsRecursive, lsMaxDepth = oldRecursive, oldDepth })

	lsRecursive = false
	lsMaxDepth = -1
	err := lsCmd.Args(lsCmd, nil)
	var negativeDepth *exitError
	if !errors.As(err, &negativeDepth) || negativeDepth.code != output.ExitArgs || !strings.Contains(err.Error(), "--max-depth must be >= 0") {
		t.Fatalf("unexpected negative max-depth result: %T %v", err, err)
	}

	lsMaxDepth = 1
	err = lsCmd.Args(lsCmd, nil)
	var requiresRecursive *exitError
	if !errors.As(err, &requiresRecursive) || requiresRecursive.code != output.ExitArgs || !strings.Contains(err.Error(), "--max-depth requires --recursive") {
		t.Fatalf("unexpected max-depth without recursive result: %T %v", err, err)
	}
}

func TestBatchCommandsExposeContinueOnError(t *testing.T) {
	commands := map[string]*cobra.Command{
		"upload":      uploadCmd,
		"download":    downloadCmd,
		"mkdir":       mkdirCmd,
		"ls":          lsCmd,
		"stat":        statCmd,
		"du":          duCmd,
		"offline add": offlineAddCmd,
		"offline rm":  offlineRmCmd,
		"sessions rm": sessionsRmCmd,
	}
	for name, command := range commands {
		if command.Flags().Lookup("continue-on-error") == nil {
			t.Fatalf("%s is missing --continue-on-error", name)
		}
	}
	if err := sessionsRmCmd.Args(sessionsRmCmd, []string{"abc", "def"}); err != nil {
		t.Fatalf("sessions rm rejected multiple IDs: %v", err)
	}
	if !strings.Contains(sessionsRmCmd.Use, "...") {
		t.Fatalf("sessions rm help does not advertise multiple IDs: %q", sessionsRmCmd.Use)
	}
}

func TestTransferBatchCommandsExposeJobs(t *testing.T) {
	for name, command := range map[string]*cobra.Command{"upload": uploadCmd, "download": downloadCmd} {
		flag := command.Flags().Lookup("jobs")
		if flag == nil {
			t.Fatalf("%s is missing --jobs", name)
		}
		if flag.DefValue != "1" {
			t.Fatalf("%s --jobs default: got %q want 1", name, flag.DefValue)
		}
	}
}

func TestTransferCommandsExposeDryRun(t *testing.T) {
	for name, command := range map[string]*cobra.Command{"upload": uploadCmd, "download": downloadCmd, "share download": shareDownloadCmd} {
		flag := command.Flags().Lookup("dry-run")
		if flag == nil {
			t.Fatalf("%s is missing --dry-run", name)
		}
		if flag.DefValue != "false" {
			t.Fatalf("%s --dry-run default: got %q want false", name, flag.DefValue)
		}
		if !strings.Contains(command.Long, "without creating") {
			t.Fatalf("%s help does not explain dry-run no-write semantics", name)
		}
	}
}

func TestMutationCommandsExposeDryRun(t *testing.T) {
	commands := map[string]*cobra.Command{
		"cp":              cpCmd,
		"mv":              mvCmd,
		"rm":              rmCmd,
		"mkdir":           mkdirCmd,
		"offline add":     offlineAddCmd,
		"offline rm":      offlineRmCmd,
		"offline clear":   offlineClearCmd,
		"recycle restore": recycleRestoreCmd,
		"sessions rm":     sessionsRmCmd,
		"sync":            syncCmd,
	}
	for name, command := range commands {
		flag := command.Flags().Lookup("dry-run")
		if flag == nil {
			t.Fatalf("%s is missing --dry-run", name)
		}
		if flag.DefValue != "false" {
			t.Fatalf("%s --dry-run default: got %q want false", name, flag.DefValue)
		}
	}
}

func TestSyncPolicyFlagsHaveSafeDefaults(t *testing.T) {
	direction := syncCmd.Flags().Lookup("direction")
	if direction == nil || direction.DefValue != syncDirectionBoth {
		t.Fatalf("sync --direction default: %#v", direction)
	}
	conflict := syncCmd.Flags().Lookup("conflict")
	if conflict == nil || conflict.DefValue != syncConflictError {
		t.Fatalf("sync --conflict default: %#v", conflict)
	}
	allowDestructive := syncCmd.Flags().Lookup("allow-destructive")
	check := syncCmd.Flags().Lookup("check")
	if check == nil || check.DefValue != "false" {
		t.Fatalf("sync --check default: %#v", check)
	}
	if allowDestructive == nil || allowDestructive.DefValue != "false" {
		t.Fatalf("sync --allow-destructive default: %#v", allowDestructive)
	}
	continueOnError := syncCmd.Flags().Lookup("continue-on-error")
	if continueOnError == nil || continueOnError.DefValue != "false" {
		t.Fatalf("sync --continue-on-error default: %#v", continueOnError)
	}
	maxErrors := syncCmd.Flags().Lookup("max-errors")
	if maxErrors == nil || maxErrors.DefValue != "0" {
		t.Fatalf("sync --max-errors default: %#v", maxErrors)
	}
	deleteFlag := syncCmd.Flags().Lookup("delete")
	if deleteFlag == nil || deleteFlag.DefValue != "false" {
		t.Fatalf("sync --delete default: %#v", deleteFlag)
	}
	expectPlan := syncCmd.Flags().Lookup("expect-plan")
	if expectPlan == nil || expectPlan.DefValue != "" {
		t.Fatalf("sync --expect-plan default: %#v", expectPlan)
	}
	resume := syncCmd.Flags().Lookup("resume")
	if resume == nil || resume.DefValue != "" {
		t.Fatalf("sync --resume default: %#v", resume)
	}
	noJournal := syncCmd.Flags().Lookup("no-journal")
	if noJournal == nil || noJournal.DefValue != "false" {
		t.Fatalf("sync --no-journal default: %#v", noJournal)
	}
	maxDeleteRoots := syncCmd.Flags().Lookup("max-delete-roots")
	if maxDeleteRoots == nil || maxDeleteRoots.DefValue != "0" {
		t.Fatalf("sync --max-delete-roots default: %#v", maxDeleteRoots)
	}
	maxDeleteItems := syncCmd.Flags().Lookup("max-delete-items")
	if maxDeleteItems == nil || maxDeleteItems.DefValue != "0" {
		t.Fatalf("sync --max-delete-items default: %#v", maxDeleteItems)
	}
	maxDeleteBytes := syncCmd.Flags().Lookup("max-delete-bytes")
	if maxDeleteBytes == nil || maxDeleteBytes.DefValue != "" {
		t.Fatalf("sync --max-delete-bytes default: %#v", maxDeleteBytes)
	}
	jobs := syncCmd.Flags().Lookup("jobs")
	if jobs == nil || jobs.DefValue != "1" {
		t.Fatalf("sync --jobs default: %#v", jobs)
	}
	if !strings.Contains(syncCmd.Long, "--check") || !strings.Contains(syncCmd.Long, "fully converged") || !strings.Contains(syncCmd.Long, "replace-remote/replace-local") || !strings.Contains(syncCmd.Long, "--delete") || !strings.Contains(syncCmd.Long, "--expect-plan") || !strings.Contains(syncCmd.Long, "--resume") || !strings.Contains(syncCmd.Long, "--no-journal") || !strings.Contains(syncCmd.Long, "recovery-required") || !strings.Contains(syncCmd.Long, "sync journal inspect") || !strings.Contains(syncCmd.Long, "mixed whole-tree preflight") || !strings.Contains(syncCmd.Long, "--max-delete-roots") || !strings.Contains(syncCmd.Long, "--max-delete-items") || !strings.Contains(syncCmd.Long, "--max-delete-bytes") || !strings.Contains(syncCmd.Long, "--continue-on-error") || !strings.Contains(syncCmd.Long, "--max-errors") || !strings.Contains(syncCmd.Long, "already-running wave") || !strings.Contains(syncCmd.Long, "blocked") || !strings.Contains(syncCmd.Long, "deterministic plan_id") || !strings.Contains(syncCmd.Long, "requires --direction upload or download") || !strings.Contains(syncCmd.Long, "remote recycle bin") || !strings.Contains(syncCmd.Long, "not atomic") || !strings.Contains(syncCmd.Long, "whole-tree read-only preflight") || !strings.Contains(syncCmd.Long, "zero processed actions") || !strings.Contains(syncCmd.Long, "dependency DAG") || !strings.Contains(syncCmd.Long, "workers-per-interface") || !strings.Contains(syncCmd.Long, "failed execution wave") {
		t.Fatalf("sync help does not explain policy/preflight/parallel safety: %q", syncCmd.Long)
	}
}

func TestSyncJournalHelpDocumentsSchemaMigrationSafety(t *testing.T) {
	if !strings.Contains(syncJournalCmd.Long, "legacy readable schemas") || !strings.Contains(syncJournalCmd.Long, "atomically migrate") || !strings.Contains(syncJournalCmd.Long, "newer schema versions fail closed") {
		t.Fatalf("sync journal help does not explain schema compatibility: %q", syncJournalCmd.Long)
	}
	if syncJournalMigrateCmd.Args == nil || !strings.Contains(syncJournalMigrateCmd.Long, "never executes sync data actions") || !strings.Contains(syncJournalMigrateCmd.Long, "atomic replacement") {
		t.Fatalf("sync journal migrate help does not explain migration safety: %q", syncJournalMigrateCmd.Long)
	}
}

func TestBatchCommandsExposeFromFile(t *testing.T) {
	commands := map[string]*cobra.Command{
		"upload":      uploadCmd,
		"download":    downloadCmd,
		"cp":          cpCmd,
		"mv":          mvCmd,
		"rm":          rmCmd,
		"mkdir":       mkdirCmd,
		"stat":        statCmd,
		"du":          duCmd,
		"offline add": offlineAddCmd,
		"offline rm":  offlineRmCmd,
		"sessions rm": sessionsRmCmd,
	}
	for name, command := range commands {
		flag := command.Flags().Lookup("from-file")
		if flag == nil {
			t.Fatalf("%s is missing --from-file", name)
		}
		if flag.DefValue != "" {
			t.Fatalf("%s --from-file default: got %q want empty", name, flag.DefValue)
		}
	}
}
