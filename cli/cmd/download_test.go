package cmd

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveDownloadTargetPath_UsesExplicitFilePath(t *testing.T) {
	got := resolveDownloadTargetPath(filepath.Join("/tmp", "custom-name.mp4"), "remote-name.mp4")
	want := filepath.Join("/tmp", "custom-name.mp4")
	if got != want {
		t.Fatalf("unexpected target path: got %q want %q", got, want)
	}
}

func TestResolveDownloadTargetPath_AppendsNameForDirectoryHint(t *testing.T) {
	directoryHint := filepath.Join(t.TempDir(), "downloads") + string(filepath.Separator)
	got := resolveDownloadTargetPath(directoryHint, "remote-name.mp4")
	want := filepath.Join(directoryHint, "remote-name.mp4")
	if got != want {
		t.Fatalf("unexpected target path: got %q want %q", got, want)
	}
}

func TestResolveDownloadTargetPath_TreatsNonExistingExtensionlessPathAsFile(t *testing.T) {
	got := resolveDownloadTargetPath(filepath.Join("/tmp", "LICENSE"), "remote-name.mp4")
	want := filepath.Join("/tmp", "LICENSE")
	if got != want {
		t.Fatalf("unexpected target path: got %q want %q", got, want)
	}
}

func TestDownloadHTTPClientDefaultTimeoutAllowsLargeDownloads(t *testing.T) {
	if defaultDownloadTimeout < time.Hour {
		t.Fatalf("expected default timeout to allow large downloads, got %s", defaultDownloadTimeout)
	}
}

func TestValidateDownloadTimeoutRejectsNegativeTimeout(t *testing.T) {
	if err := validateDownloadTimeout(-time.Second); err == nil {
		t.Fatal("expected negative timeout to be rejected")
	}
}

func TestValidateDownloadTimeoutAllowsZeroToDisableTimeout(t *testing.T) {
	if err := validateDownloadTimeout(0); err != nil {
		t.Fatalf("expected zero timeout to be accepted: %v", err)
	}
}
