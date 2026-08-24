package buildinfo

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasePackagingContractR16IntegrationFreeze(t *testing.T) {
	root := releaseContractRepoRoot(t)
	audit := readReleaseContractFile(t, root, filepath.Join("docs", "release", "v0.2.0", "V0.2.0_RC1_INTEGRATION_AUDIT.md"))
	for _, needle := range []string{
		"Baseline: published `v0.1.4`",
		"Planned candidate: `v0.2.0-rc.1`",
		"CLI Cobra `Use` names | 21 | 42 | 0",
		"CLI flag names | 22 | 53 | 0",
		"MCP tool names | 11 | 49 | 0",
		"MCP server flag names | 9 | 11 | 0",
		"`pkg/driver` exported funcs/methods/types scan | 218 | 223 | 0",
		"`ListOptions` is the one known source exception",
		"`get_download_info` was historically always registered; it now requires `--allow-sensitive-tools`",
		"current schema version: `2`",
		"minimum readable schema version: `1`",
		"registered migration edge: `v1 -> v2`",
		"No real `v0.2.0-rc.1` tag or GitHub release has been created",
		"release-ready-race` remains an Ubuntu 24.04 CI/release authority",
	} {
		if !strings.Contains(audit, needle) {
			t.Errorf("R16 integration audit missing %q", needle)
		}
	}

	notes := readReleaseContractFile(t, root, filepath.Join("docs", "release", "v0.2.0", "RELEASE_NOTES_V0.2.0_RC1.md"))
	for _, needle := range []string{
		"pre-release draft",
		"v0.2.0-rc.1",
		"all 21 historical Cobra `Use` names",
		"all 22 historical flag names",
		"All 11 historical MCP tool names",
		"all 9 historical MCP server flags",
		"zero removals relative to `v0.1.4`",
		"DownloadByShareCodeRequest",
		"external unkeyed literal",
		"six platform archives, six SPDX 2.3 SBOMs",
	} {
		if !strings.Contains(notes, needle) {
			t.Errorf("R16 release notes missing %q", needle)
		}
	}

	compat := readReleaseContractFile(t, root, filepath.Join("pkg", "driver", "public_api_compat_test.go"))
	for _, needle := range []string{
		"driver.QRCodeStatusResp{",
		"driver.OfflineTaskResponse{\"\", \"\"}",
		"func() driver.SharedDownloadInfo",
		"driver.FileStatResponse{",
		"driver.ListOptions{ApiURLs:",
	} {
		if !strings.Contains(compat, needle) {
			t.Errorf("R16 public source compatibility pin missing %q", needle)
		}
	}

	download := readReleaseContractFile(t, root, filepath.Join("pkg", "driver", "download.go"))
	for _, needle := range []string{
		"type SharedDownloadInfo struct {",
		"type SharedDownloadRequest struct {",
		"DownloadByShareCodeRequest(",
		"DownloadByShareCodeRequestWithUA(",
		"Header http.Header `json:\"-\"`",
	} {
		if !strings.Contains(download, needle) {
			t.Errorf("R16 share download additive compatibility contract missing %q", needle)
		}
	}

	legacyMCP := readReleaseContractFile(t, root, filepath.Join("mcp", "main.go"))
	for _, needle := range []string{"mcpapp.Main()", "historical ./mcp source entry point"} {
		if !strings.Contains(legacyMCP, needle) {
			t.Errorf("legacy ./mcp entrypoint compatibility contract missing %q", needle)
		}
	}

	schema := readReleaseContractFile(t, root, filepath.Join("internal", "syncjournal", "schema.go"))
	for _, needle := range []string{
		"Version            = 2",
		"MinReadableVersion = 1",
		"LayoutVersion      = \"v1\"",
		"SchemaID           = \"115driver.sync-journal\"",
	} {
		if !strings.Contains(schema, needle) {
			t.Errorf("R16 sync-journal compatibility contract missing %q", needle)
		}
	}

	migration := readReleaseContractFile(t, root, filepath.Join("cli", "cmd", "sync_journal_migrate.go"))
	if !strings.Contains(migration, "1: migrateSyncJournalV1ToV2") {
		t.Fatal("R16 requires the explicit v1 -> v2 sync-journal migration edge")
	}
}
