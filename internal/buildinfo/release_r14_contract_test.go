package buildinfo

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasePackagingContractR14DryRunWorkflow(t *testing.T) {
	root := releaseContractRepoRoot(t)
	workflow := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "release-dry-run.yml"))
	for _, needle := range []string{
		"workflow_dispatch:",
		"release_tag:",
		"permissions:\n  contents: read",
		"group: release-dry-run-${{ github.ref }}",
		"runs-on: ubuntu-24.04",
		"uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
		"uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff",
		"go-version: \"1.23.4\"",
		"Inspect actual tag and GitHub release state",
		"gh api --paginate --slurp",
		"$RUNNER_TEMP/release-api.json",
		"-releases-file \"$RUNNER_TEMP/release-api.json\"",
		"for state in absent draft published; do",
		"-simulate-state \"$state\"",
		"make release-ready-race",
		"uses: anchore/sbom-action/download-syft@e22c389904149dbc22b58101806040fa8d37a610",
		"syft-version: v1.50.0",
		"Create local-only proposed release tag",
		"git tag \"$RELEASE_TAG\" \"$GITHUB_SHA\"",
		"uses: goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a",
		"args: release --clean --skip=publish --parallelism=1",
		"make verify-release-artifacts EXPECTED_ARTIFACT_VERSION=\"${RELEASE_TAG#v}\"",
		"go run ./cmd/release-notes -tag \"$RELEASE_TAG\"",
		"Assert dry-run authority remains read-only",
		"-eq 6",
		"-eq 12",
	} {
		if !strings.Contains(workflow, needle) {
			t.Errorf("release dry-run workflow contract missing %q", needle)
		}
	}
	if got := strings.Count(workflow, "goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a"); got != 1 {
		t.Fatalf("release dry-run must build exactly one non-publishing final-version candidate, got %d GoReleaser actions", got)
	}
	for _, forbidden := range []string{
		"contents: write",
		"id-token: write",
		"attestations: write",
		"artifact-metadata: write",
		"actions/attest@",
		"gh release create",
		"gh release edit",
		"gh release upload",
		"gh api -X POST",
		"gh api -X PATCH",
		"gh api -X DELETE",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("release dry-run unexpectedly contains mutating authority %q", forbidden)
		}
	}
}

func TestReleasePackagingContractR14PrereleaseChannel(t *testing.T) {
	root := releaseContractRepoRoot(t)
	release := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "changelog.yml"))
	for _, needle := range []string{
		"Resolve release version and channel",
		"release tag must be SemVer",
		"prerelease=false",
		"grep -Eq '^v(0|[1-9][0-9]*)",
		"channel_version=\"${version%%+*}\"",
		"prerelease_part=\"${channel_version#*-}\"",
		"grep -Eq '^0[0-9]+$'",
		"numeric prerelease identifier with a leading zero",
		"echo \"prerelease=$prerelease\" >> \"$GITHUB_OUTPUT\"",
		"--json isPrerelease --jq '.isPrerelease'",
		"--prerelease=${{ steps.release_version.outputs.prerelease }}",
	} {
		if !strings.Contains(release, needle) {
			t.Errorf("tag release prerelease contract missing %q", needle)
		}
	}
	if got := strings.Count(release, "--prerelease=${{ steps.release_version.outputs.prerelease }}"); got != 2 {
		t.Fatalf("tag release must set the expected prerelease bit on draft create and repair, got %d occurrences", got)
	}
	if got := strings.Count(release, "--json isPrerelease --jq '.isPrerelease'"); got != 3 {
		t.Fatalf("tag release must verify prerelease metadata on published-rerun, staged draft, and published final state, got %d occurrences", got)
	}
}

func TestReleasePackagingContractR14OperationsGateAndChecklist(t *testing.T) {
	root := releaseContractRepoRoot(t)
	makefile := readReleaseContractFile(t, root, "Makefile")
	for _, needle := range []string{
		"test-release-ops:",
		"$(GO) test -count=1 ./internal/releaseops",
		"$(GO) run ./cmd/release-preflight",
		"for state in absent draft published",
		"test-release-packaging: test-release-entrypoints test-release-notes-cli test-release-ops",
	} {
		if !strings.Contains(makefile, needle) {
			t.Errorf("Makefile R14 release operations gate missing %q", needle)
		}
	}

	artifactCheck := readReleaseContractFile(t, root, filepath.Join("cmd", "release-artifact-check", "main.go"))
	if !strings.Contains(artifactCheck, "install_smoke=%t") || !strings.Contains(artifactCheck, "report.InstallSmoked") {
		t.Fatal("release artifact checker must report archive installation smoke explicitly")
	}

	checklist := readReleaseContractFile(t, root, filepath.Join("docs", "release", "v0.2.0", "RELEASE_CHECKLIST.md"))
	for _, needle := range []string{
		"# Release Candidate Checklist",
		"release-dry-run",
		"contents: read",
		"v0.1.5-rc.1",
		"thirteen assets",
		"six archives",
		"six SPDX SBOMs",
		"published release must take the read-only verification path",
		"gh attestation verify <artifact> -R OWNER/REPO",
	} {
		if !strings.Contains(checklist, needle) {
			t.Errorf("release candidate checklist missing %q", needle)
		}
	}
}
