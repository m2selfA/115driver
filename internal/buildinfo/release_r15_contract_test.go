package buildinfo

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasePackagingContractR15LineagePreflight(t *testing.T) {
	root := releaseContractRepoRoot(t)
	release := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "changelog.yml"))
	for _, needle := range []string{
		"gh api --paginate --slurp",
		"$RUNNER_TEMP/release-api.json",
		"Verify release lineage preflight",
		"go run ./cmd/release-preflight",
		"-tag \"$GITHUB_REF_NAME\"",
		"-expected-sha \"$GITHUB_SHA\"",
		"-releases-file \"$RUNNER_TEMP/release-api.json\"",
		"$RUNNER_TEMP/release-preflight.json",
	} {
		if !strings.Contains(release, needle) {
			t.Errorf("tag release R15 lineage contract missing %q", needle)
		}
	}
	stateIndex := strings.Index(release, "Inspect existing release state")
	setupIndex := strings.Index(release, "Set up pinned release Go")
	lineageIndex := strings.Index(release, "Verify release lineage preflight")
	gateIndex := strings.Index(release, "Run complete release readiness gate")
	if stateIndex < 0 || setupIndex < stateIndex || lineageIndex < setupIndex || gateIndex < lineageIndex {
		t.Fatal("new/draft tag workflow must inspect remote state -> set up Go -> verify release lineage -> run release readiness")
	}
	if strings.Contains(release, "if: steps.release_state.outputs.state == 'published'\n        run: go run ./cmd/release-preflight") {
		t.Fatal("already-published reruns must stay on the no-Go read-only verification path")
	}
}

func TestReleasePackagingContractR15DryRunUsesRealLineage(t *testing.T) {
	root := releaseContractRepoRoot(t)
	workflow := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "release-dry-run.yml"))
	for _, needle := range []string{
		"for example v0.2.0-rc.1",
		"-simulate-state \"$state\"",
		"-releases-file \"$RUNNER_TEMP/release-api.json\"",
	} {
		if !strings.Contains(workflow, needle) {
			t.Errorf("release dry-run R15 lineage contract missing %q", needle)
		}
	}
	if got := strings.Count(workflow, "-releases-file \"$RUNNER_TEMP/release-api.json\""); got != 2 {
		t.Fatalf("dry-run must use the same real release history for actual and simulated preflight, got %d references", got)
	}
}

func TestReleasePackagingContractR15CandidateLineAndPromotionDocs(t *testing.T) {
	root := releaseContractRepoRoot(t)
	makefile := readReleaseContractFile(t, root, "Makefile")
	for _, needle := range []string{
		"\"tag_name\":\"v9.9.8\"",
		"-simulate-state \"$$state\" -releases-file \"$$tmp/releases.json\"",
	} {
		if !strings.Contains(makefile, needle) {
			t.Errorf("Makefile R15 release-ops smoke missing %q", needle)
		}
	}

	checklist := readReleaseContractFile(t, root, "RELEASE_CHECKLIST.md")
	for _, needle := range []string{
		"`v0.2.0-rc.1`",
		"`latest_published_tag`",
		"strictly higher SemVer precedence",
		"`promotion_from`",
		"same `MAJOR.MINOR.PATCH` core",
	} {
		if !strings.Contains(checklist, needle) {
			t.Errorf("R15 release checklist missing %q", needle)
		}
	}

	readme := readReleaseContractFile(t, root, "README.md")
	for _, needle := range []string{
		"`v0.2.0-rc.1`",
		"`latest_published_tag`",
		"`promotion_from`",
		"exact already-published tag remains a read-only rerun exception",
	} {
		if !strings.Contains(readme, needle) {
			t.Errorf("README R15 release lineage documentation missing %q", needle)
		}
	}
}
