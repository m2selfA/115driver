package cmd

import "testing"

func TestImportantUploadStatusSuppressesRoutineProgressMessages(t *testing.T) {
	for _, message := range []string{
		"Preparing upload...",
		"Checking 115 rapid upload...",
		"Using 7 interface(s) for OSS upload...",
		"Using 1 interface in sequential compatibility mode...",
	} {
		if importantUploadStatus(message) {
			t.Errorf("routine progress message should not be logged on non-TTY output: %q", message)
		}
	}
	for _, message := range []string{
		"Network warning: adapter disappeared",
		"Warning: upload succeeded but resume state cleanup failed",
		"Recovering upload; retry 1/3 after: connection reset",
		"[2/42] file.bin — Recovering upload; retry 2/3 in single-interface sequential compatibility mode after: verification failed",
	} {
		if !importantUploadStatus(message) {
			t.Errorf("important recovery message should remain visible on non-TTY output: %q", message)
		}
	}
}
