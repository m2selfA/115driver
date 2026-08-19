package server

import (
	"context"
	"testing"
	"time"
)

func TestNewServerPreservesDownloadTransferDefaults(t *testing.T) {
	s := NewServer()
	if s.downloadTransfer.Interfaces != "auto" || s.downloadTransfer.Strategy != "file" {
		t.Fatalf("unexpected default transfer config: %#v", s.downloadTransfer)
	}
	if s.downloadTransfer.WorkersPerInterface != 1 || s.downloadTransfer.ProbeCacheTTL != 15*time.Minute || s.downloadTransfer.Retries != 3 {
		t.Fatalf("unexpected default transfer tuning: %#v", s.downloadTransfer)
	}
	if s.downloadTransfer.HealthCooldown != 5*time.Second || s.downloadTransfer.HealthCooldownMax != time.Minute {
		t.Fatalf("unexpected default health tuning: %#v", s.downloadTransfer)
	}
	if !s.downloadTransfer.Resume || s.downloadTransfer.URLRefreshes != 3 {
		t.Fatalf("unexpected default P9 resume/refresh tuning: %#v", s.downloadTransfer)
	}
}

func TestServerRejectsInvalidTransferConfigBeforeStartingTransport(t *testing.T) {
	config := DefaultDownloadTransferConfig()
	config.Strategy = "future"
	s := NewServer().WithDownloadTransferConfig(config)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected invalid transfer config to fail before MCP transport starts")
	}
}
