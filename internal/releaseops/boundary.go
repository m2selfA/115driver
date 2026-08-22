package releaseops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	CommitManifestSchema = "115driver.rc-commit-manifest/v1"
	CommitClassifierR17  = "r17-v1"
)

type CommitManifest struct {
	Schema        string        `json:"schema"`
	BaseTag       string        `json:"base_tag"`
	CandidateTag  string        `json:"candidate_tag"`
	Classifier    string        `json:"classifier"`
	TotalPaths    int           `json:"total_paths"`
	PathSetSHA256 string        `json:"path_set_sha256"`
	Layers        []CommitLayer `json:"layers"`
}

type CommitLayer struct {
	ID            string `json:"id"`
	Subject       string `json:"subject"`
	Count         int    `json:"count"`
	PathSetSHA256 string `json:"path_set_sha256"`
}

type CommitLayerReport struct {
	ID            string   `json:"id"`
	Subject       string   `json:"subject"`
	Count         int      `json:"count"`
	PathSetSHA256 string   `json:"path_set_sha256"`
	Paths         []string `json:"paths,omitempty"`
}

type CommitBoundaryReport struct {
	BaseTag       string              `json:"base_tag"`
	CandidateTag  string              `json:"candidate_tag"`
	Classifier    string              `json:"classifier"`
	TotalPaths    int                 `json:"total_paths"`
	PathSetSHA256 string              `json:"path_set_sha256"`
	Layers        []CommitLayerReport `json:"layers"`
}

type commitLayerSpec struct {
	ID      string
	Subject string
}

var r17CommitLayerSpecs = []commitLayerSpec{
	{ID: "driver", Subject: "feat(driver): harden API validation and read-only controls"},
	{ID: "core", Subject: "feat(core): add durable transfer and sync recovery engines"},
	{ID: "cli", Subject: "feat(cli): add batch transfer sync and maintenance workflows"},
	{ID: "mcp", Subject: "feat(mcp): add typed batch planning and recovery tools"},
	{ID: "release", Subject: "feat(release): certify reproducible rerun-safe RC promotion"},
}

var r17ReleaseDocumentationContractPaths = map[string]struct{}{
	"mcp/server/alias_repair_documentation_contract_test.go":  {},
	"mcp/server/documentation_contract_test.go":               {},
	"mcp/server/read_snapshot_documentation_contract_test.go": {},
}

func NormalizeCommitPaths(paths []string) ([]string, error) {
	set := make(map[string]struct{}, len(paths))
	for _, raw := range paths {
		normalized, err := normalizeCommitPath(raw)
		if err != nil {
			return nil, err
		}
		set[normalized] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for item := range set {
		result = append(result, item)
	}
	sort.Strings(result)
	return result, nil
}

func HashCommitPaths(paths []string) (string, []string, error) {
	normalized, err := NormalizeCommitPaths(paths)
	if err != nil {
		return "", nil, err
	}
	var payload strings.Builder
	for _, item := range normalized {
		payload.WriteString(item)
		payload.WriteByte('\n')
	}
	digest := sha256.Sum256([]byte(payload.String()))
	return hex.EncodeToString(digest[:]), normalized, nil
}

func ClassifyRCCommitPath(raw string) (string, error) {
	item, err := normalizeCommitPath(raw)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(item, "pkg/driver/") {
		return "driver", nil
	}
	if isR17CorePath(item) {
		return "core", nil
	}
	if strings.HasPrefix(item, "cli/") {
		return "cli", nil
	}
	if _, ok := r17ReleaseDocumentationContractPaths[item]; ok {
		return "release", nil
	}
	if strings.HasPrefix(item, "mcp/") || strings.HasPrefix(item, "internal/mcpapp/") || strings.HasPrefix(item, "cmd/115driver-mcp-server/") {
		return "mcp", nil
	}
	return "release", nil
}

func ValidateCommitManifest(manifest CommitManifest) error {
	if manifest.Schema != CommitManifestSchema {
		return fmt.Errorf("commit manifest schema %q, want %q", manifest.Schema, CommitManifestSchema)
	}
	if strings.TrimSpace(manifest.BaseTag) == "" || strings.TrimSpace(manifest.CandidateTag) == "" {
		return fmt.Errorf("commit manifest base_tag and candidate_tag are required")
	}
	if manifest.Classifier != CommitClassifierR17 {
		return fmt.Errorf("commit manifest classifier %q, want %q", manifest.Classifier, CommitClassifierR17)
	}
	if manifest.TotalPaths <= 0 {
		return fmt.Errorf("commit manifest total_paths must be positive")
	}
	if err := validateCommitPathSetSHA("commit manifest", manifest.PathSetSHA256); err != nil {
		return err
	}
	if len(manifest.Layers) != len(r17CommitLayerSpecs) {
		return fmt.Errorf("commit manifest has %d layers, want %d", len(manifest.Layers), len(r17CommitLayerSpecs))
	}
	total := 0
	for index, expected := range r17CommitLayerSpecs {
		layer := manifest.Layers[index]
		if layer.ID != expected.ID {
			return fmt.Errorf("commit manifest layer %d id %q, want %q", index, layer.ID, expected.ID)
		}
		if layer.Subject != expected.Subject {
			return fmt.Errorf("commit manifest layer %s subject %q, want %q", layer.ID, layer.Subject, expected.Subject)
		}
		if layer.Count <= 0 {
			return fmt.Errorf("commit manifest layer %s count must be positive", layer.ID)
		}
		if err := validateCommitPathSetSHA("commit manifest layer "+layer.ID, layer.PathSetSHA256); err != nil {
			return err
		}
		total += layer.Count
	}
	if total != manifest.TotalPaths {
		return fmt.Errorf("commit manifest layer counts sum to %d, want total_paths %d", total, manifest.TotalPaths)
	}
	return nil
}

func EvaluateCommitBoundary(manifest CommitManifest, candidatePaths []string) (CommitBoundaryReport, error) {
	if err := ValidateCommitManifest(manifest); err != nil {
		return CommitBoundaryReport{}, err
	}
	fullHash, normalized, err := HashCommitPaths(candidatePaths)
	if err != nil {
		return CommitBoundaryReport{}, err
	}
	report := CommitBoundaryReport{
		BaseTag: manifest.BaseTag, CandidateTag: manifest.CandidateTag, Classifier: manifest.Classifier,
		TotalPaths: len(normalized), PathSetSHA256: fullHash,
		Layers: make([]CommitLayerReport, len(manifest.Layers)),
	}
	for index, layer := range manifest.Layers {
		report.Layers[index] = CommitLayerReport{ID: layer.ID, Subject: layer.Subject}
	}
	layerIndex := make(map[string]int, len(report.Layers))
	for index, layer := range report.Layers {
		layerIndex[layer.ID] = index
	}
	for _, item := range normalized {
		layerID, err := ClassifyRCCommitPath(item)
		if err != nil {
			return report, err
		}
		index, ok := layerIndex[layerID]
		if !ok {
			return report, fmt.Errorf("classifier returned unknown layer %q for %q", layerID, item)
		}
		report.Layers[index].Paths = append(report.Layers[index].Paths, item)
	}
	for index := range report.Layers {
		hash, layerPaths, err := HashCommitPaths(report.Layers[index].Paths)
		if err != nil {
			return report, err
		}
		report.Layers[index].Paths = layerPaths
		report.Layers[index].Count = len(layerPaths)
		report.Layers[index].PathSetSHA256 = hash
	}
	if report.TotalPaths != manifest.TotalPaths || report.PathSetSHA256 != manifest.PathSetSHA256 {
		return report, fmt.Errorf("RC path set mismatch: count=%d sha256=%s; want count=%d sha256=%s", report.TotalPaths, report.PathSetSHA256, manifest.TotalPaths, manifest.PathSetSHA256)
	}
	for index, expected := range manifest.Layers {
		actual := report.Layers[index]
		if actual.Count != expected.Count || actual.PathSetSHA256 != expected.PathSetSHA256 {
			return report, fmt.Errorf("RC layer %s mismatch: count=%d sha256=%s; want count=%d sha256=%s", actual.ID, actual.Count, actual.PathSetSHA256, expected.Count, expected.PathSetSHA256)
		}
	}
	return report, nil
}

func EvaluateCommitIndexLayer(report CommitBoundaryReport, layerID string, stagedPaths []string) (CommitLayerReport, error) {
	layerID = strings.TrimSpace(layerID)
	if layerID == "" {
		return CommitLayerReport{}, fmt.Errorf("RC Git index layer is required")
	}
	var expected *CommitLayerReport
	for index := range report.Layers {
		if report.Layers[index].ID == layerID {
			expected = &report.Layers[index]
			break
		}
	}
	if expected == nil {
		return CommitLayerReport{}, fmt.Errorf("unknown RC commit layer %q", layerID)
	}
	hash, normalized, err := HashCommitPaths(stagedPaths)
	if err != nil {
		return CommitLayerReport{}, err
	}
	actual := CommitLayerReport{
		ID: layerID, Subject: expected.Subject, Count: len(normalized), PathSetSHA256: hash, Paths: normalized,
	}
	if actual.Count != expected.Count || actual.PathSetSHA256 != expected.PathSetSHA256 {
		return actual, fmt.Errorf("RC Git index layer %s mismatch: count=%d sha256=%s; want count=%d sha256=%s", layerID, actual.Count, actual.PathSetSHA256, expected.Count, expected.PathSetSHA256)
	}
	return actual, nil
}

func normalizeCommitPath(raw string) (string, error) {
	item := strings.TrimSpace(raw)
	if item == "" {
		return "", fmt.Errorf("commit path is empty")
	}
	if strings.ContainsRune(item, '\x00') {
		return "", fmt.Errorf("commit path contains NUL")
	}
	if filepath.IsAbs(item) || filepath.VolumeName(item) != "" {
		return "", fmt.Errorf("commit path %q must be repository-relative", raw)
	}
	item = filepath.ToSlash(item)
	if strings.HasPrefix(item, "/") {
		return "", fmt.Errorf("commit path %q must be repository-relative", raw)
	}
	item = path.Clean(item)
	if item == "." || item == ".." || strings.HasPrefix(item, "../") {
		return "", fmt.Errorf("commit path %q escapes repository root", raw)
	}
	return item, nil
}

func isR17CorePath(item string) bool {
	for _, prefix := range []string{
		"internal/transfer/",
		"internal/upload/",
		"internal/sessionconfig/",
		"internal/remoteresolver/",
		"internal/remotetree/",
		"internal/syncexec/",
		"internal/syncguard/",
		"internal/syncjournal/",
		"internal/syncplan/",
	} {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	switch item {
	case "internal/buildinfo/version.go", "internal/buildinfo/version_test.go", "go.mod":
		return true
	default:
		return false
	}
}

func validateCommitPathSetSHA(label, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s path_set_sha256 must be 64 lowercase hex characters", label)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s path_set_sha256 is invalid: %w", label, err)
	}
	return nil
}
