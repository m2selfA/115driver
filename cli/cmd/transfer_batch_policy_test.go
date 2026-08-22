package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestValidateStaticTransferBatchPolicy(t *testing.T) {
	newCommand := func(fromFile string) *cobra.Command {
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

	cmd := newCommand("")
	if err := validateStaticTransferBatchPolicy(cmd, []string{"source", "destination"}, 2, "uploads"); err == nil || !strings.Contains(err.Error(), "multi-source uploads") {
		t.Fatalf("single-source jobs policy = %v", err)
	}

	cmd = newCommand("")
	if err := validateStaticTransferBatchPolicy(cmd, []string{"a", "b", "destination"}, 2, "uploads"); err == nil || !strings.Contains(err.Error(), "--continue-on-error") {
		t.Fatalf("parallel failure policy = %v", err)
	}
	if err := cmd.Flags().Set("continue-on-error", "true"); err != nil {
		t.Fatal(err)
	}
	if err := validateStaticTransferBatchPolicy(cmd, []string{"a", "b", "destination"}, 2, "uploads"); err != nil {
		t.Fatalf("explicit multi-source parallel batch rejected: %v", err)
	}

	cmd = newCommand("sources.txt")
	if err := validateStaticTransferBatchPolicy(cmd, []string{"source", "destination"}, 2, "uploads"); err != nil {
		t.Fatalf("from-file cardinality should be deferred to runtime: %v", err)
	}
}

func TestTransferInputArgsRejectKnownSingleSourceParallelism(t *testing.T) {
	oldUploadTimeout, oldUploadWorkers, oldUploadChunk, oldUploadContents, oldUploadRecursive := uploadTimeout, uploadWorkersPerInterface, uploadChunkSize, uploadContents, uploadRecursive
	oldDownloadTimeout, oldDownloadWorkers, oldDownloadStrategy, oldDownloadChunk, oldDownloadSession, oldDownloadRecursive := downloadTimeout, downloadWorkersPerInterface, downloadStrategy, downloadChunkSize, downloadSession, downloadRecursive
	t.Cleanup(func() {
		uploadTimeout, uploadWorkersPerInterface, uploadChunkSize, uploadContents, uploadRecursive = oldUploadTimeout, oldUploadWorkers, oldUploadChunk, oldUploadContents, oldUploadRecursive
		downloadTimeout, downloadWorkersPerInterface, downloadStrategy, downloadChunkSize, downloadSession, downloadRecursive = oldDownloadTimeout, oldDownloadWorkers, oldDownloadStrategy, oldDownloadChunk, oldDownloadSession, oldDownloadRecursive
	})

	uploadTimeout = 0
	uploadWorkersPerInterface = 0
	uploadChunkSize = ""
	uploadContents = false
	uploadRecursive = false
	downloadTimeout = defaultDownloadTimeout
	downloadWorkersPerInterface = 0
	downloadStrategy = ""
	downloadChunkSize = ""
	downloadSession = ""
	downloadRecursive = false

	for name, validate := range map[string]func(*cobra.Command) error{
		"upload":   func(cmd *cobra.Command) error { return uploadInputArgs(cmd, []string{"local.bin", "/remote"}) },
		"download": func(cmd *cobra.Command) error { return downloadInputArgs(cmd, []string{"/remote/file", "./local"}) },
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newTransferPreauthTestCommand(t)
			if err := cmd.Flags().Set("jobs", "2"); err != nil {
				t.Fatal(err)
			}
			requireExitArgs(t, validate(cmd))
		})
	}
}
