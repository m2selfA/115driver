package cmd

import (
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
)

func TestBuildUploadOptionsUsesTransferDefaultsAndOverrides(t *testing.T) {
	config := auth.DefaultTransferConfig()
	options, err := buildUploadOptions(config, "Ethernet,2", "64MiB", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if options.Interfaces != "Ethernet,2" || options.ChunkSize != 64<<20 || options.Retries != config.Retries || options.Timeout != 3*time.Hour {
		t.Fatalf("unexpected upload options: %#v", options)
	}
	if options.HealthTracker == nil {
		t.Fatal("upload options did not create P8 health tracker")
	}
}

func TestBuildUploadOptionsRejectsUnsupportedWorkerCountAndChunkSize(t *testing.T) {
	config := auth.DefaultTransferConfig()
	config.WorkersPerInterface = 2
	if _, err := buildUploadOptions(config, "", "", time.Hour); err == nil {
		t.Fatal("expected multiple workers per physical interface to be rejected")
	}
	config = auth.DefaultTransferConfig()
	if _, err := buildUploadOptions(config, "", "1.5MiB", time.Hour); err == nil {
		t.Fatal("expected invalid byte-size syntax to fail")
	}
	if _, err := buildUploadOptions(config, "", "64KiB", time.Hour); err == nil {
		t.Fatal("expected OSS part size below 100KiB to fail")
	}
	if _, err := buildUploadOptions(config, "", "", -time.Second); err == nil {
		t.Fatal("expected negative upload timeout to fail")
	}
}
