package cmd

import "testing"

func TestRootAndVersionCommandShareBuildVersion(t *testing.T) {
	if got, want := rootCmd.Version, currentVersion(); got != want {
		t.Fatalf("root --version = %q, version command = %q", got, want)
	}
}
