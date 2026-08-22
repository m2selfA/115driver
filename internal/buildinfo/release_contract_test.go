package buildinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseContractRepoRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve release contract source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func readReleaseContractFile(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.ToSlash(rel), err)
	}
	return string(body)
}

func TestReleasePackagingContractPinsDualEntrypointsAndMatrix(t *testing.T) {
	root := releaseContractRepoRoot(t)
	goreleaser := readReleaseContractFile(t, root, ".goreleaser.yml")
	for _, needle := range []string{
		"main: ./cmd/115driver",
		"mod_timestamp: \"{{ .CommitTimestamp }}\"",
		"builds_info:",
		"mtime: \"{{ .CommitDate }}\"",
		"owner: root",
		"group: root",
		"mode: 0644",
		"binary: 115driver",
		"github.com/SheltonZhu/115driver/cli/cmd.version={{.Version}}",
		"main: ./cmd/115driver-mcp-server",
		"binary: 115driver-mcp-server",
		"github.com/SheltonZhu/115driver/mcp/server.version={{.Version}}",
		"CGO_ENABLED=0",
		"- linux",
		"- darwin",
		"- windows",
		"- amd64",
		"- arm64",
		"formats:",
		"- tar.gz",
		"wrap_in_directory: false",
		"sboms:",
		"artifacts: archive",
		"{{ .ArtifactName }}.spdx.json",
		"cmd: syft",
		"spdx-json=${document}",
		"disable: false",
		"algorithm: sha256",
	} {
		if !strings.Contains(goreleaser, needle) {
			t.Errorf(".goreleaser.yml does not pin release contract %q", needle)
		}
	}

	if got := strings.Count(goreleaser, "mod_timestamp: \"{{ .CommitTimestamp }}\""); got != 2 {
		t.Fatalf(".goreleaser.yml must pin both build mtimes to the commit timestamp, got %d", got)
	}
	if got := strings.Count(goreleaser, "mtime: \"{{ .CommitDate }}\""); got != 3 {
		t.Fatalf(".goreleaser.yml must pin archive binary/README/LICENSE mtimes to the commit date, got %d", got)
	}

	makefile := readReleaseContractFile(t, root, "Makefile")
	attributes := readReleaseContractFile(t, root, ".gitattributes")
	for _, needle := range []string{"go.mod text eol=lf", "go.sum text eol=lf"} {
		if !strings.Contains(attributes, needle) {
			t.Errorf(".gitattributes does not pin module metadata line endings %q", needle)
		}
	}
	for _, needle := range []string{
		"SYNC_SESSION_CERT_COUNT ?= 5",
		"test-sync-session-cert: test-sync-journal-alias-cert",
		"test-sync-session-race: test-sync-journal-alias-race",
		"test-release-packaging:",
		"test-release-entrypoints:",
		"test-release-notes-cli:",
		"verify-release-artifacts:",
		"$(GO) run ./cmd/release-artifact-check",
		"$(GO) install -trimpath",
		"release-ready:",
		"release-ready-race: release-ready",
		"$(GO) mod verify",
		"$(GO) test -count=1 $(PKG)",
		"$(GO) mod tidy -diff",
		"$(MAKE) test-sync-session-cert",
		"$(MAKE) test-sync-session-race",
		"RELEASE_GOOS ?= linux darwin windows",
		"RELEASE_GOARCH ?= amd64 arm64",
		"./internal/syncexec",
		"./internal/syncguard",
		"CGO_ENABLED=0 GOOS=\"$$os\" GOARCH=\"$$arch\"",
		"$(CLI_MAIN)",
		"$(MCP_MAIN)",
	} {
		if !strings.Contains(makefile, needle) {
			t.Errorf("Makefile does not pin sync/session release contract %q", needle)
		}
	}
}

func TestReleasePackagingContractRunsBeforeGoReleaser(t *testing.T) {
	root := releaseContractRepoRoot(t)
	testWorkflow := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "test.yml"))
	for _, needle := range []string{
		"make release-ready-race",
		"Build GoReleaser release-candidate snapshot",
		"runs-on: ubuntu-24.04",
		"Set up pinned release Go",
		"go-version: \"1.23.4\"",
		"version: \"v2.17.1\"",
		"uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
		"uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff",
		"uses: anchore/sbom-action/download-syft@e22c389904149dbc22b58101806040fa8d37a610",
		"syft-version: v1.50.0",
		"uses: goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a",
		"args: release --snapshot --clean --skip=publish --parallelism=1",
		"make verify-release-artifacts",
		"Preserve first snapshot archive checksums",
		"grep '\\.tar\\.gz$' dist/checksums.txt",
		".release-archive-checksums.first",
		"Rebuild GoReleaser snapshot for reproducibility",
		".release-archive-checksums.second",
		"cmp .release-archive-checksums.first .release-archive-checksums.second",
		"Verify reproducible GoReleaser snapshot archives",
	} {
		if !strings.Contains(testWorkflow, needle) {
			t.Errorf("test workflow does not run release artifact gate %q", needle)
		}
	}
	if got := strings.Count(testWorkflow, "goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a"); got != 2 {
		t.Fatalf("PR artifact certification must build the pinned GoReleaser snapshot twice, got %d actions", got)
	}
	if strings.Contains(testWorkflow, "sbom-checksums.txt") {
		t.Fatal("PR artifact certification must use GoReleaser's single checksums.txt authority")
	}

	release := readReleaseContractFile(t, root, filepath.Join(".github", "workflows", "changelog.yml"))
	for _, needle := range []string{
		"concurrency:",
		"group: release-${{ github.ref }}",
		"cancel-in-progress: false",
		"Inspect existing release state",
		"state=absent",
		"state=draft",
		"state=published",
		"Verify existing published release",
		"if: steps.release_state.outputs.state == 'published'",
		"if: steps.release_state.outputs.state != 'published'",
		"git rev-list -n 1",
		"make release-ready-race",
		"Build GoReleaser final-version release candidate",
		"runs-on: ubuntu-24.04",
		"Set up pinned release Go",
		"go-version: \"1.23.4\"",
		"version: \"v2.17.1\"",
		"uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
		"uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff",
		"uses: anchore/sbom-action/download-syft@e22c389904149dbc22b58101806040fa8d37a610",
		"syft-version: v1.50.0",
		"uses: goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a",
		"args: release --clean --skip=publish --parallelism=1",
		"make verify-release-artifacts EXPECTED_ARTIFACT_VERSION=\"${GITHUB_REF_NAME#v}\"",
		"id-token: write",
		"attestations: write",
		"artifact-metadata: write",
		"uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6",
		"subject-checksums: ./dist/checksums.txt",
		"subject-path: ./dist/checksums.txt",
		"sbom-path:",
		"Verify release candidate provenance and SBOM attestations",
		"gh attestation verify \"$artifact\"",
		"--signer-workflow \"$GITHUB_REPOSITORY/.github/workflows/changelog.yml\"",
		"--source-digest \"$GITHUB_SHA\"",
		"--source-ref \"$GITHUB_REF\"",
		"--predicate-type https://spdx.dev/Document/v2.3",
		"Stage or repair verified release draft",
		"case \"${{ steps.release_state.outputs.state }}\" in",
		"gh release create \"$GITHUB_REF_NAME\"",
		"gh release upload \"$GITHUB_REF_NAME\"",
		"--clobber",
		"dist/*.tar.gz",
		"dist/*.spdx.json",
		"dist/checksums.txt",
		"--draft",
		"--verify-tag",
		"--notes-file .release-notes.md",
		"Verify staged release assets",
		"gh release view \"$GITHUB_REF_NAME\" --json isDraft",
		"gh release download \"$GITHUB_REF_NAME\"",
		"-eq 13",
		"-eq 12",
		"cmp dist/checksums.txt \"$tmp/checksums.txt\"",
		"sha256sum -c checksums.txt",
		"Publish verified release draft",
		"gh release edit \"$GITHUB_REF_NAME\" --draft=false --verify-tag",
		"Verify published release metadata",
		"gh release view \"$GITHUB_REF_NAME\" --json tagName",
	} {
		if !strings.Contains(release, needle) {
			t.Errorf("release workflow does not freeze verified artifact promotion %q", needle)
		}
	}
	if got := strings.Count(release, "goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a"); got != 1 {
		t.Fatalf("tag release must build artifacts exactly once with GoReleaser, got %d actions", got)
	}
	if got := strings.Count(release, "sbom-path:"); got != 6 {
		t.Fatalf("tag release must bind one SPDX SBOM attestation to each archive, got %d", got)
	}
	if got := strings.Count(release, "actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6"); got != 8 {
		t.Fatalf("tag release must use 2 generic provenance attestations plus 6 archive SBOM attestations, got %d", got)
	}
	for _, forbidden := range []string{
		"actions/checkout@v",
		"actions/setup-go@v",
		"goreleaser/goreleaser-action@v",
		"actions/attest@v",
		"anchore/sbom-action/download-syft@v",
		"sbom-checksums.txt",
		"Run GoReleaser",
		"args: release --clean --release-notes=.release-notes.md",
	} {
		if strings.Contains(release, forbidden) {
			t.Fatalf("tag release contains unpinned or rebuilding release surface %q", forbidden)
		}
	}
	stateIndex := strings.Index(release, "Inspect existing release state")
	setupIndex := strings.Index(release, "Set up pinned release Go")
	gateIndex := strings.Index(release, "make release-ready-race")
	syftIndex := strings.Index(release, "Download pinned Syft")
	candidateIndex := strings.Index(release, "Build GoReleaser final-version release candidate")
	verifyIndex := strings.Index(release, "make verify-release-artifacts")
	attestIndex := strings.Index(release, "Attest verified release archives")
	provenanceIndex := strings.Index(release, "Verify release candidate provenance and SBOM attestations")
	notesIndex := strings.Index(release, "Generate release notes")
	stageIndex := strings.Index(release, "Stage or repair verified release draft")
	roundTripIndex := strings.Index(release, "Verify staged release assets")
	publishIndex := strings.Index(release, "Publish verified release draft")
	publishedMetadataIndex := strings.Index(release, "Verify published release metadata")
	if stateIndex < 0 || setupIndex < stateIndex || gateIndex < setupIndex || syftIndex < gateIndex || candidateIndex < syftIndex || verifyIndex < candidateIndex || attestIndex < verifyIndex || provenanceIndex < attestIndex || notesIndex < provenanceIndex || stageIndex < notesIndex || roundTripIndex < stageIndex || publishIndex < roundTripIndex || publishedMetadataIndex < publishIndex {
		t.Fatalf("tag release order must be state inspection -> conditional toolchain/readiness -> Syft -> single final-version build -> verification -> attestation -> notes -> rerun-safe draft staging -> remote byte verification -> publication -> metadata check")
	}
}

func TestReleasePackagingContractPinsR17CommitBoundary(t *testing.T) {
	root := releaseContractRepoRoot(t)
	manifest := readReleaseContractFile(t, root, "V0.2.0_RC1_COMMIT_MANIFEST.json")
	for _, needle := range []string{
		`"schema": "115driver.rc-commit-manifest/v1"`,
		`"base_tag": "v0.1.4"`,
		`"candidate_tag": "v0.2.0-rc.1"`,
		`"classifier": "r17-v1"`,
		`"total_paths": 402`,
		`"path_set_sha256": "3d152170cad105d2e95bcb30a80c2d759dbd48c1c0ca4b3e4a6a998c0c5e3095"`,
		`"count": 46`,
		`"count": 96`,
		`"count": 103`,
		`"count": 125`,
		`"count": 32`,
		`"path_set_sha256": "f6dd744711d510b4934b32a2902cb34ccf934a3ffac90ca0247e41128a62ca43"`,
		`"path_set_sha256": "0bfa8bc9ee015dde5a6c59380a366549fe6c5f0d2537909dba1e0eb273a9ed0c"`,
		`"path_set_sha256": "b27501b536b078f44aab94b1ecfe1dccbd283e2f2182239710d5cafdec2a1a95"`,
		`"path_set_sha256": "d428b77b3f42e417b0549e1a9dd8fd1fb5174c5c5d1a33f3d7fea834b9daa662"`,
		`"path_set_sha256": "914d4f7858a7ada999457e49af9b182e676b1f2370983c2e9019a602ae263bcc"`,
		`feat(driver): harden API validation and read-only controls`,
		`feat(core): add durable transfer and sync recovery engines`,
		`feat(cli): add batch transfer sync and maintenance workflows`,
		`feat(mcp): add typed batch planning and recovery tools`,
		`feat(release): certify reproducible rerun-safe RC promotion`,
	} {
		if !strings.Contains(manifest, needle) {
			t.Errorf("R17 commit manifest missing %q", needle)
		}
	}

	boundary := readReleaseContractFile(t, root, filepath.Join("internal", "releaseops", "boundary.go"))
	for _, needle := range []string{
		`CommitManifestSchema = "115driver.rc-commit-manifest/v1"`,
		`CommitClassifierR17  = "r17-v1"`,
		`mcp/server/alias_repair_documentation_contract_test.go`,
		`mcp/server/documentation_contract_test.go`,
		`mcp/server/read_snapshot_documentation_contract_test.go`,
		`func EvaluateCommitBoundary(`,
		`func ClassifyRCCommitPath(`,
		`func EvaluateCommitIndexLayer(`,
	} {
		if !strings.Contains(boundary, needle) {
			t.Errorf("R17 boundary implementation missing %q", needle)
		}
	}

	checker := readReleaseContractFile(t, root, filepath.Join("cmd", "release-boundary-check", "main.go"))
	for _, needle := range []string{
		`flag.Bool("print-layer-nul"`,
		`flag.String("verify-index-layer"`,
		`"diff", "--cached", "--name-only", "--no-renames"`,
		`"diff", "--name-only", "--no-renames", baseTag + "..HEAD"`,
	} {
		if !strings.Contains(checker, needle) {
			t.Errorf("R18 index boundary checker missing %q", needle)
		}
	}

	makefile := readReleaseContractFile(t, root, "Makefile")
	for _, needle := range []string{
		"RC_COMMIT_MANIFEST ?= V0.2.0_RC1_COMMIT_MANIFEST.json",
		"verify-rc-commit-boundary:",
		"$(GO) run ./cmd/release-boundary-check -manifest '$(RC_COMMIT_MANIFEST)'",
		"test-release-packaging: test-release-entrypoints test-release-notes-cli test-release-ops verify-rc-commit-boundary",
		"RC_COMMIT_LAYER ?=",
		"verify-rc-index-empty:",
		"-verify-index-layer empty",
		"verify-rc-staged-layer:",
		"-verify-index-layer '$(RC_COMMIT_LAYER)'",
	} {
		if !strings.Contains(makefile, needle) {
			t.Errorf("R17 Makefile boundary contract missing %q", needle)
		}
	}
}

func TestGitHubActionsDependencyUpdateContract(t *testing.T) {
	root := releaseContractRepoRoot(t)
	dependabot := readReleaseContractFile(t, root, filepath.Join(".github", "dependabot.yml"))
	for _, needle := range []string{
		"package-ecosystem: github-actions",
		"directory: /",
		"interval: weekly",
		"release-actions:",
		"patterns:",
		"- \"*\"",
		"open-pull-requests-limit: 5",
		"prefix: \"chore(deps)\"",
	} {
		if !strings.Contains(dependabot, needle) {
			t.Errorf("Dependabot GitHub Actions contract missing %q", needle)
		}
	}
}
