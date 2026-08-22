package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

func configureCLIUploadProgress(options *uploadpkg.Options, sourceLabel string) func() {
	if options == nil || jsonOutput {
		return func() {}
	}
	if batchParallelActive() {
		options.Progress = func(message string) {
			if importantUploadStatus(message) {
				fmt.Fprintf(os.Stderr, "[%s] %s\n", sourceLabel, message)
			}
		}
		return func() {}
	}
	progress := output.NewTransferProgress()
	options.Progress = func(message string) {
		progress.SetStatus(message)
		if !progress.Enabled() && importantUploadStatus(message) {
			fmt.Fprintln(os.Stderr, message)
		}
	}
	options.ProgressBytes = progress.SetProgress
	return progress.Finish
}

func importantUploadStatus(message string) bool {
	message = strings.TrimSpace(message)
	return strings.HasPrefix(message, "Network warning:") ||
		strings.HasPrefix(message, "Warning:") ||
		strings.Contains(message, "Recovering upload;") ||
		strings.Contains(message, "retry ")
}
