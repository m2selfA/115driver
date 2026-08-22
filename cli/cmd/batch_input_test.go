package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func newBatchInputTestCommand(t *testing.T, fromFile string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	addContinueOnErrorFlag(cmd)
	addBatchJobsFlag(cmd)
	addBatchFromFileFlag(cmd)
	if fromFile != "" {
		if err := cmd.Flags().Set("from-file", fromFile); err != nil {
			t.Fatal(err)
		}
	}
	return cmd
}

func TestReadBatchSourcesFromStdinStripsBOMAndPreservesPathWhitespace(t *testing.T) {
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader("\ufefffirst path\r\n\r\n second path \r\nthird\n"))
	sources, err := readBatchSources(cmd, "-")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first path", " second path ", "third"}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("unexpected stdin sources: got %#v want %#v", sources, want)
	}
}

func TestExpandTransferSourceArgsAppendsFileSourcesBeforeDestination(t *testing.T) {
	root := t.TempDir()
	listPath := filepath.Join(root, "sources.txt")
	if err := os.WriteFile(listPath, []byte("from-file-a\nfrom-file-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newBatchInputTestCommand(t, listPath)
	got, err := expandTransferSourceArgs(cmd, []string{"explicit-a", "destination"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"explicit-a", "from-file-a", "from-file-b", "destination"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected expanded args: got %#v want %#v", got, want)
	}
}

func TestExpandTransferSourceArgsRejectsEmptySourceFileWithoutExplicitSources(t *testing.T) {
	listPath := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(listPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newBatchInputTestCommand(t, listPath)
	_, err := expandTransferSourceArgs(cmd, []string{"destination"})
	if err == nil || !strings.Contains(err.Error(), "did not provide any sources") {
		t.Fatalf("unexpected empty-list result: %v", err)
	}
}

func TestExpandBatchInputArgsCombinesExplicitAndFileInputs(t *testing.T) {
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader("file-a\nfile-b\n"))
	got, err := expandBatchInputArgs(cmd, []string{"explicit-a"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"explicit-a", "file-a", "file-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected expanded batch inputs: got %#v want %#v", got, want)
	}
}

func TestExpandLSInputArgsDefaultsToRootAndSupportsFromFile(t *testing.T) {
	withoutFile := newBatchInputTestCommand(t, "")
	got, err := expandLSInputArgs(withoutFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"/"}) {
		t.Fatalf("ls default inputs = %#v, want root", got)
	}

	withFile := newBatchInputTestCommand(t, "-")
	withFile.SetIn(strings.NewReader("/a\n/b\n"))
	got, err = expandLSInputArgs(withFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Fatalf("ls from-file inputs = %#v", got)
	}
}

func TestBatchInputArgsAllowsFromFileOnlyInvocation(t *testing.T) {
	withoutFile := newBatchInputTestCommand(t, "")
	if err := batchInputArgs(withoutFile, nil); err == nil {
		t.Fatal("empty invocation succeeded without --from-file")
	}
	withFile := newBatchInputTestCommand(t, "-")
	if err := batchInputArgs(withFile, nil); err != nil {
		t.Fatalf("from-file-only invocation rejected: %v", err)
	}
}

func TestReadBatchSourcesRejectsNULWithLineNumber(t *testing.T) {
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader("ok\nbad\x00path\n"))
	_, err := readBatchSources(cmd, "-")
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("unexpected NUL error: %v", err)
	}
}

func TestTransferSourceArgsAllowsDestinationOnlyWithFromFile(t *testing.T) {
	withoutFile := newBatchInputTestCommand(t, "")
	if err := transferSourceArgs(withoutFile, []string{"destination"}); err == nil {
		t.Fatal("destination-only invocation succeeded without --from-file")
	}
	withFile := newBatchInputTestCommand(t, "-")
	if err := transferSourceArgs(withFile, []string{"destination"}); err != nil {
		t.Fatalf("destination-only invocation rejected with --from-file: %v", err)
	}
	if err := transferSourceArgs(withFile, nil); err == nil {
		t.Fatal("--from-file invocation accepted a missing destination")
	}
}

func TestRunUploadCommandFromFileUsesSingleRunnerForOneSource(t *testing.T) {
	oldRun := uploadSingleRunE
	t.Cleanup(func() { uploadSingleRunE = oldRun })
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader("only.bin\n"))
	var got []string
	uploadSingleRunE = func(_ *cobra.Command, args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	if err := runUploadCommand(cmd, []string{"/remote"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"only.bin", "/remote"}) {
		t.Fatalf("single runner received %#v", got)
	}
}

func TestRunUploadCommandFromFileDispatchesMultipleSourcesWithoutReentry(t *testing.T) {
	oldRun := uploadSingleRunE
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		uploadSingleRunE = oldRun
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
		jsonOutput, printer = oldJSON, oldPrinter
	})
	uploadRecursive = false
	uploadContents = false
	uploadSession = ""
	jsonOutput = true
	printer = output.NewPrinter(false)

	root := t.TempDir()
	first := filepath.Join(root, "a.bin")
	second := filepath.Join(root, "b.bin")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader(first + "\n" + second + "\n"))
	var calls [][]string
	uploadSingleRunE = func(_ *cobra.Command, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	if err := runUploadCommand(cmd, []string{"/remote"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != first || calls[1][0] != second {
		t.Fatalf("unexpected batch single-runner calls: %#v", calls)
	}
}

func TestRmFromStdinRequiresForceBeforeReadingOrResolvingInputs(t *testing.T) {
	oldForce := rmForce
	t.Cleanup(func() { rmForce = oldForce })
	rmForce = false
	cmd := newBatchInputTestCommand(t, "-")
	cmd.SetIn(strings.NewReader("/some/path\n"))
	err := rmCmd.RunE(cmd, nil)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("unexpected rm stdin safety result: %T %v", err, err)
	}
}

func TestBatchSingleRunnerBindingsDoNotPointAtTopLevelDispatch(t *testing.T) {
	if uploadSingleRunE == nil || downloadSingleRunE == nil {
		t.Fatal("single runners were not initialized")
	}
	if reflect.ValueOf(uploadSingleRunE).Pointer() == reflect.ValueOf(uploadCmd.RunE).Pointer() {
		t.Fatal("upload batch single runner re-enters top-level upload dispatch")
	}
	if reflect.ValueOf(downloadSingleRunE).Pointer() == reflect.ValueOf(downloadCmd.RunE).Pointer() {
		t.Fatal("download batch single runner re-enters top-level download dispatch")
	}
}

func TestRunUploadCommandReportsFromFileReadFailureAsArgsError(t *testing.T) {
	cmd := newBatchInputTestCommand(t, filepath.Join(t.TempDir(), "missing.txt"))
	err := runUploadCommand(cmd, []string{"/remote"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "read batch source file") {
		t.Fatalf("unexpected from-file read failure: %T %v", err, err)
	}
}
