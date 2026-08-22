package releaseops

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestClassifyRCCommitPathR17(t *testing.T) {
	for path, want := range map[string]string{
		"pkg/driver/download.go":                                  "driver",
		"internal/syncjournal/store.go":                           "core",
		"internal/buildinfo/version.go":                           "core",
		"go.mod":                                                  "core",
		"cli/cmd/sync.go":                                         "cli",
		"mcp/server/tools/plan_sync.go":                           "mcp",
		"internal/mcpapp/app.go":                                  "mcp",
		"cmd/115driver-mcp-server/main.go":                        "mcp",
		"mcp/server/documentation_contract_test.go":               "release",
		"mcp/server/alias_repair_documentation_contract_test.go":  "release",
		"mcp/server/read_snapshot_documentation_contract_test.go": "release",
		"README.md":                          "release",
		"internal/releaseops/boundary.go":    "release",
		"cmd/release-boundary-check/main.go": "release",
		"V0.2.0_RC1_COMMIT_MANIFEST.json":    "release",
	} {
		got, err := ClassifyRCCommitPath(path)
		if err != nil {
			t.Fatalf("ClassifyRCCommitPath(%q): %v", path, err)
		}
		if got != want {
			t.Errorf("ClassifyRCCommitPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHashCommitPathsNormalizesSortsAndDeduplicates(t *testing.T) {
	got, paths, err := HashCommitPaths([]string{"b", "a", "./a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "a,b" {
		t.Fatalf("normalized paths = %#v", paths)
	}
	wantDigest := sha256.Sum256([]byte("a\nb\n"))
	if want := hex.EncodeToString(wantDigest[:]); got != want {
		t.Fatalf("path set hash = %s, want %s", got, want)
	}
}

func TestEvaluateCommitIndexLayerUsesExactFrozenSet(t *testing.T) {
	paths := []string{
		"pkg/driver/download.go",
		"internal/syncjournal/store.go",
		"cli/cmd/sync.go",
		"mcp/server/tools/plan_sync.go",
		"README.md",
	}
	manifest := syntheticCommitManifest(t, paths)
	report, err := EvaluateCommitBoundary(manifest, paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateCommitIndexLayer(report, "cli", []string{"cli/cmd/sync.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 || got.Paths[0] != "cli/cmd/sync.go" {
		t.Fatalf("unexpected index report: %#v", got)
	}
	if _, err := EvaluateCommitIndexLayer(report, "cli", []string{"cli/cmd/sync.go", "README.md"}); err == nil || !strings.Contains(err.Error(), "RC Git index layer cli mismatch") {
		t.Fatalf("extra staged path error = %v", err)
	}
	if _, err := EvaluateCommitIndexLayer(report, "missing", nil); err == nil || !strings.Contains(err.Error(), "unknown RC commit layer") {
		t.Fatalf("unknown layer error = %v", err)
	}
}

func TestEvaluateCommitBoundaryUsesExactLayerSets(t *testing.T) {
	paths := []string{
		"pkg/driver/download.go",
		"internal/syncjournal/store.go",
		"cli/cmd/sync.go",
		"mcp/server/tools/plan_sync.go",
		"README.md",
	}
	manifest := syntheticCommitManifest(t, paths)
	report, err := EvaluateCommitBoundary(manifest, append(paths, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalPaths != 5 || len(report.Layers) != 5 {
		t.Fatalf("unexpected boundary report: %#v", report)
	}
	if _, err := EvaluateCommitBoundary(manifest, append(paths, "NEW_FEATURE.txt")); err == nil || !strings.Contains(err.Error(), "RC path set mismatch") {
		t.Fatalf("extra candidate path error = %v", err)
	}
}

func TestValidateCommitManifestRejectsLayerOrderAndCountDrift(t *testing.T) {
	paths := []string{
		"pkg/driver/download.go",
		"internal/syncjournal/store.go",
		"cli/cmd/sync.go",
		"mcp/server/tools/plan_sync.go",
		"README.md",
	}
	manifest := syntheticCommitManifest(t, paths)
	manifest.Layers[0], manifest.Layers[1] = manifest.Layers[1], manifest.Layers[0]
	if err := ValidateCommitManifest(manifest); err == nil || !strings.Contains(err.Error(), "layer 0 id") {
		t.Fatalf("layer order error = %v", err)
	}
	manifest = syntheticCommitManifest(t, paths)
	manifest.Layers[0].Count++
	if err := ValidateCommitManifest(manifest); err == nil || !strings.Contains(err.Error(), "layer counts sum") {
		t.Fatalf("layer count error = %v", err)
	}
}

func TestNormalizeCommitPathsRejectsRepositoryEscape(t *testing.T) {
	for _, path := range []string{"", "../outside", "a/../../outside"} {
		if _, err := NormalizeCommitPaths([]string{path}); err == nil {
			t.Fatalf("NormalizeCommitPaths(%q) unexpectedly succeeded", path)
		}
	}
}

func syntheticCommitManifest(t *testing.T, paths []string) CommitManifest {
	t.Helper()
	fullHash, normalized, err := HashCommitPaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	byLayer := make(map[string][]string, len(r17CommitLayerSpecs))
	for _, item := range normalized {
		layer, err := ClassifyRCCommitPath(item)
		if err != nil {
			t.Fatal(err)
		}
		byLayer[layer] = append(byLayer[layer], item)
	}
	manifest := CommitManifest{
		Schema: CommitManifestSchema, BaseTag: "v0.1.4", CandidateTag: "v0.2.0-rc.1", Classifier: CommitClassifierR17,
		TotalPaths: len(normalized), PathSetSHA256: fullHash,
	}
	for _, spec := range r17CommitLayerSpecs {
		hash, layerPaths, err := HashCommitPaths(byLayer[spec.ID])
		if err != nil {
			t.Fatal(err)
		}
		manifest.Layers = append(manifest.Layers, CommitLayer{ID: spec.ID, Subject: spec.Subject, Count: len(layerPaths), PathSetSHA256: hash})
	}
	return manifest
}
