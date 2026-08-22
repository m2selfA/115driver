package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPTransferPlanChecksumBytes = int64(4 << 30)
	maxMCPTransferPlanChecksumBytes     = int64(64 << 30)
)

// PlanTransferArgs builds one read-only preflight plan from local uploads and
// ordinary 115 downloads. Inputs retain the same shapes as the existing batch
// transfer tools so planning and execution preflight cannot silently diverge.
type PlanTransferArgs struct {
	Uploads          []UploadFromLocalFileItem `json:"uploads,omitempty" jsonschema:"local upload items using the same local_path, dir_id, and optional file_name contract as upload_from_local_files"`
	Downloads        []DownloadFileArgs        `json:"downloads,omitempty" jsonschema:"download items using the same pick_code, local_path, and optional user_agent contract as download_files"`
	MaxChecksumBytes int64                     `json:"max_checksum_bytes,omitempty" jsonschema:"maximum aggregate local bytes hashed into content snapshots, including upload sources and existing download targets; default 4 GiB, maximum 64 GiB"`
}

// MCPTransferPlanItem is the safe projection of one transfer operation. Local
// paths, pick codes, signed URLs, request headers, and content digests are never
// included; hidden identities are instead bound through opaque preconditions in
// MCPPlan.
type MCPTransferPlanItem struct {
	OperationID  string             `json:"operation_id" jsonschema:"operation ID in the MCPPlan envelope"`
	Index        int                `json:"index" jsonschema:"zero-based index within the direction-specific input array"`
	Direction    string             `json:"direction" jsonschema:"upload or download"`
	FileName     string             `json:"file_name" jsonschema:"validated file name"`
	DirID        string             `json:"dir_id,omitempty" jsonschema:"validated 115 target directory ID for uploads"`
	FileSize     *int64             `json:"file_size,omitempty" jsonschema:"known transfer size in bytes, including zero; omitted when unknown"`
	TargetExists bool               `json:"target_exists,omitempty" jsonschema:"whether a download target already exists and may be replaced"`
	SafetyClass  MCPPlanSafetyClass `json:"safety_class" jsonschema:"additive or destructive effect if this operation is later executed"`
}

// MCPTransferPlanSummary describes the bounded mixed transfer plan without
// exposing any source credential or local filesystem identity.
type MCPTransferPlanSummary struct {
	Requested            int   `json:"requested"`
	Uploads              int   `json:"uploads"`
	Downloads            int   `json:"downloads"`
	ExistingLocalTargets int   `json:"existing_local_targets"`
	KnownTransferBytes   int64 `json:"known_transfer_bytes"`
	UnknownSizeTransfers int   `json:"unknown_size_transfers"`
	ChecksummedFiles     int   `json:"checksummed_files"`
	ChecksummedBytes     int64 `json:"checksummed_bytes"`
}

// MCPTransferPlanOutput combines the generic MCPPlan v1 with safe transfer
// metadata. It is a planning artifact only; no file data is transferred.
type MCPTransferPlanOutput struct {
	Summary MCPTransferPlanSummary `json:"summary"`
	Plan    MCPPlan                `json:"plan"`
	Items   []MCPTransferPlanItem  `json:"items" jsonschema:"safe transfer operations in uploads-then-downloads order"`
}

type mcpTransferLocalTargetIdentity struct {
	localPath string
	canonical string
	exists    bool
	info      os.FileInfo
}

type mcpTransferLocalTargetSnapshot struct {
	exists bool
	token  string
}

func mcpTransferOpaqueToken(domain string, values ...string) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write([]byte(value))
	}
	write("115driver-mcp-transfer-plan-v1")
	write(domain)
	for _, value := range values {
		write(value)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func mcpTransferUploadSnapshot(item mcpPreparedLocalUpload) (string, error) {
	if item.file == nil {
		return "", fmt.Errorf("local upload source is not open")
	}
	before, err := item.file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat local upload source: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() != item.fileSize {
		return "", fmt.Errorf("local upload source changed after preflight")
	}
	canonical, err := filepath.EvalSymlinks(item.file.Name())
	if err != nil {
		return "", fmt.Errorf("resolve local upload source: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	pathBefore, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat canonical local upload source: %w", err)
	}
	if !os.SameFile(before, pathBefore) {
		return "", fmt.Errorf("local upload source identity changed after preflight")
	}
	if _, err := item.file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek local upload source for snapshot: %w", err)
	}
	h := sha256.New()
	written, err := io.Copy(h, item.file)
	if err != nil {
		return "", fmt.Errorf("hash local upload source: %w", err)
	}
	if written != before.Size() {
		return "", fmt.Errorf("local upload source size changed while hashing")
	}
	after, err := item.file.Stat()
	if err != nil {
		return "", fmt.Errorf("restat local upload source: %w", err)
	}
	pathAfter, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("restat canonical local upload source: %w", err)
	}
	if !os.SameFile(before, after) || !os.SameFile(before, pathAfter) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("local upload source changed while hashing")
	}
	if _, err := item.file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("restore local upload source position: %w", err)
	}
	contentDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	return mcpTransferOpaqueToken(
		"local-upload-snapshot",
		canonical,
		strconv.FormatInt(before.Size(), 10),
		contentDigest,
	), nil
}

func mcpTransferDownloadSourceSnapshot(item mcpDownloadBatchTransferItem) (string, error) {
	if item.info == nil {
		return "", fmt.Errorf("download source metadata is unavailable")
	}
	stableID := strings.TrimSpace(item.stableID)
	if stableID == "" {
		return "", fmt.Errorf("download source identity is empty")
	}
	return mcpTransferOpaqueToken(
		"remote-download-snapshot",
		stableID,
		item.info.FileName,
		strconv.FormatInt(int64(item.info.FileSize), 10),
	), nil
}

func inspectMCPTransferDownloadTarget(localPath string) (mcpTransferLocalTargetIdentity, error) {
	canonical, err := canonicalMCPDownloadBatchPathKey(localPath)
	if err != nil {
		return mcpTransferLocalTargetIdentity{}, err
	}
	info, err := os.Stat(localPath)
	if os.IsNotExist(err) {
		return mcpTransferLocalTargetIdentity{localPath: localPath, canonical: canonical}, nil
	}
	if err != nil {
		return mcpTransferLocalTargetIdentity{}, fmt.Errorf("stat local download target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return mcpTransferLocalTargetIdentity{}, fmt.Errorf("existing download target is not a regular file")
	}
	canonicalInfo, err := os.Stat(canonical)
	if err != nil {
		return mcpTransferLocalTargetIdentity{}, fmt.Errorf("stat canonical local download target: %w", err)
	}
	if !os.SameFile(info, canonicalInfo) {
		return mcpTransferLocalTargetIdentity{}, fmt.Errorf("local download target identity changed during inspection")
	}
	return mcpTransferLocalTargetIdentity{localPath: localPath, canonical: canonical, exists: true, info: info}, nil
}

func snapshotMCPTransferDownloadTarget(identity mcpTransferLocalTargetIdentity) (mcpTransferLocalTargetSnapshot, error) {
	if !identity.exists {
		canonical, err := canonicalMCPDownloadBatchPathKey(identity.localPath)
		if err != nil {
			return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("recheck absent local download target identity: %w", err)
		}
		if canonical != identity.canonical {
			return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("local download target identity changed after checksum preflight")
		}
		if _, err := os.Stat(identity.localPath); err == nil {
			return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("local download target appeared after checksum preflight")
		} else if !os.IsNotExist(err) {
			return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("recheck absent local download target: %w", err)
		}
		return mcpTransferLocalTargetSnapshot{token: mcpTransferOpaqueToken("local-download-target", identity.canonical, "absent")}, nil
	}
	if identity.info == nil || !identity.info.Mode().IsRegular() {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("existing download target snapshot is invalid")
	}
	file, err := os.Open(identity.canonical)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("open local download target for snapshot: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("stat opened local download target: %w", err)
	}
	if !os.SameFile(identity.info, opened) || opened.Size() != identity.info.Size() {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("local download target changed before hashing")
	}
	h := sha256.New()
	written, err := io.Copy(h, file)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("hash local download target: %w", err)
	}
	if written != identity.info.Size() {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("local download target size changed while hashing")
	}
	after, err := file.Stat()
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("restat opened local download target: %w", err)
	}
	pathAfter, err := os.Stat(identity.localPath)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("restat local download target: %w", err)
	}
	canonicalAfter, err := os.Stat(identity.canonical)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("restat canonical local download target: %w", err)
	}
	canonicalKeyAfter, err := canonicalMCPDownloadBatchPathKey(identity.localPath)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("recheck local download target identity after hashing: %w", err)
	}
	if canonicalKeyAfter != identity.canonical || !os.SameFile(identity.info, after) || !os.SameFile(identity.info, pathAfter) || !os.SameFile(identity.info, canonicalAfter) || after.Size() != identity.info.Size() || !after.ModTime().Equal(identity.info.ModTime()) {
		return mcpTransferLocalTargetSnapshot{}, fmt.Errorf("local download target changed while hashing")
	}
	contentDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	return mcpTransferLocalTargetSnapshot{
		exists: true,
		token: mcpTransferOpaqueToken(
			"local-download-target",
			identity.canonical,
			"present",
			strconv.FormatInt(identity.info.Size(), 10),
			contentDigest,
		),
	}, nil
}

func mcpTransferDownloadTargetSnapshot(localPath string) (mcpTransferLocalTargetSnapshot, error) {
	identity, err := inspectMCPTransferDownloadTarget(localPath)
	if err != nil {
		return mcpTransferLocalTargetSnapshot{}, err
	}
	return snapshotMCPTransferDownloadTarget(identity)
}

func appendKnownTransferBytes(summary *MCPTransferPlanSummary, size *int64) error {
	if summary == nil {
		return fmt.Errorf("transfer summary is nil")
	}
	if size == nil {
		summary.UnknownSizeTransfers++
		return nil
	}
	if *size < 0 {
		return fmt.Errorf("known transfer size must not be negative")
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if summary.KnownTransferBytes > maxInt64-*size {
		return fmt.Errorf("transfer byte estimate overflows int64")
	}
	summary.KnownTransferBytes += *size
	return nil
}

func normalizeMCPTransferChecksumBudget(value int64) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("max_checksum_bytes must be >= 0")
	}
	if value == 0 {
		value = defaultMCPTransferPlanChecksumBytes
	}
	if value > maxMCPTransferPlanChecksumBytes {
		return 0, fmt.Errorf("max_checksum_bytes must not exceed %d", maxMCPTransferPlanChecksumBytes)
	}
	return value, nil
}

func preflightMCPTransferChecksumBudget(uploads []mcpPreparedLocalUpload, targets []mcpTransferLocalTargetIdentity, maxChecksumBytes int64) (int, int64, error) {
	budget, err := normalizeMCPTransferChecksumBudget(maxChecksumBytes)
	if err != nil {
		return 0, 0, err
	}
	files := 0
	var total int64
	add := func(size int64) error {
		if size < 0 {
			return fmt.Errorf("content snapshot size must not be negative")
		}
		if total > int64(^uint64(0)>>1)-size {
			return fmt.Errorf("content snapshot byte total overflows int64")
		}
		total += size
		files++
		return nil
	}
	for i, item := range uploads {
		if item.fileSize < 0 {
			return 0, 0, fmt.Errorf("upload item %d has a negative size", i)
		}
		if err := add(item.fileSize); err != nil {
			return 0, 0, err
		}
	}
	for i, target := range targets {
		if !target.exists {
			continue
		}
		if target.info == nil {
			return 0, 0, fmt.Errorf("download target %d has no snapshot metadata", i)
		}
		if err := add(target.info.Size()); err != nil {
			return 0, 0, err
		}
	}
	if total > budget {
		return 0, 0, fmt.Errorf("local content checksum budget exceeded: need %d bytes, limit is %d", total, budget)
	}
	return files, total, nil
}

func normalizeMCPExpectedPlanID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return "", fmt.Errorf("expect_plan_id must use sha256:<64 hex> format")
	}
	raw := value[len(prefix):]
	if len(raw) != 64 {
		return "", fmt.Errorf("expect_plan_id must use sha256:<64 hex> format")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("expect_plan_id must use sha256:<64 hex> format")
	}
	return prefix + strings.ToLower(raw), nil
}

func verifyMCPExpectedTransferPlanID(expected string, output MCPTransferPlanOutput) error {
	expected, err := normalizeMCPExpectedPlanID(expected)
	if err != nil {
		return err
	}
	if expected == "" {
		return nil
	}
	if output.Plan.PlanID != expected {
		return fmt.Errorf("transfer plan no longer matches expect_plan_id; run plan_transfer again")
	}
	return nil
}

func verifyMCPPreparedTransferPlan(uploads []mcpPreparedLocalUpload, downloads []mcpDownloadBatchTransferItem, expectedPlanID string, maxChecksumBytes int64) (MCPTransferPlanOutput, error) {
	planned, err := buildMCPTransferPlan(uploads, downloads, maxChecksumBytes)
	if err != nil {
		return MCPTransferPlanOutput{}, err
	}
	if err := verifyMCPExpectedTransferPlanID(expectedPlanID, planned); err != nil {
		return MCPTransferPlanOutput{}, err
	}
	return planned, nil
}

func buildMCPTransferPlan(uploads []mcpPreparedLocalUpload, downloads []mcpDownloadBatchTransferItem, maxChecksumBytes int64) (MCPTransferPlanOutput, error) {
	targetIdentities := make([]mcpTransferLocalTargetIdentity, len(downloads))
	for i, item := range downloads {
		identity, err := inspectMCPTransferDownloadTarget(item.localPath)
		if err != nil {
			return MCPTransferPlanOutput{}, fmt.Errorf("download item %d target inspection: %w", i, err)
		}
		targetIdentities[i] = identity
	}
	checksummedFiles, checksummedBytes, err := preflightMCPTransferChecksumBudget(uploads, targetIdentities, maxChecksumBytes)
	if err != nil {
		return MCPTransferPlanOutput{}, err
	}
	output := MCPTransferPlanOutput{
		Summary: MCPTransferPlanSummary{
			Requested:        len(uploads) + len(downloads),
			Uploads:          len(uploads),
			Downloads:        len(downloads),
			ChecksummedFiles: checksummedFiles,
			ChecksummedBytes: checksummedBytes,
		},
		Items: make([]MCPTransferPlanItem, 0, len(uploads)+len(downloads)),
	}
	generic := MCPPlan{
		Kind:        "transfer",
		CreatedFrom: "plan_transfer",
		Operations:  make([]MCPPlanOperation, 0, len(uploads)+len(downloads)),
	}

	for i, item := range uploads {
		snapshot, err := mcpTransferUploadSnapshot(item)
		if err != nil {
			return MCPTransferPlanOutput{}, fmt.Errorf("upload item %d snapshot: %w", i, err)
		}
		operationID := fmt.Sprintf("upload-%06d", i)
		sourceRef := fmt.Sprintf("local-input:%06d", i)
		targetRef := "remote-dir:" + item.dirID + "/" + item.fileName
		size := item.fileSize
		generic.Operations = append(generic.Operations, MCPPlanOperation{
			ID:             operationID,
			Operation:      "upload",
			SafetyClass:    MCPPlanSafetyAdditive,
			SourceRef:      sourceRef,
			TargetRef:      targetRef,
			EstimatedBytes: &size,
		})
		generic.Preconditions = append(generic.Preconditions,
			MCPPlanPrecondition{OperationID: operationID, Kind: "local_file_snapshot", Ref: sourceRef, Expected: snapshot},
			MCPPlanPrecondition{OperationID: operationID, Kind: "remote_directory", Ref: "remote-dir:" + item.dirID, Expected: "directory"},
		)
		output.Items = append(output.Items, MCPTransferPlanItem{
			OperationID: operationID,
			Index:       i,
			Direction:   "upload",
			FileName:    item.fileName,
			DirID:       item.dirID,
			FileSize:    &size,
			SafetyClass: MCPPlanSafetyAdditive,
		})
		if err := appendKnownTransferBytes(&output.Summary, &size); err != nil {
			return MCPTransferPlanOutput{}, err
		}
	}

	for i, item := range downloads {
		sourceSnapshot, err := mcpTransferDownloadSourceSnapshot(item)
		if err != nil {
			return MCPTransferPlanOutput{}, fmt.Errorf("download item %d source snapshot: %w", i, err)
		}
		targetSnapshot, err := snapshotMCPTransferDownloadTarget(targetIdentities[i])
		if err != nil {
			return MCPTransferPlanOutput{}, fmt.Errorf("download item %d target snapshot: %w", i, err)
		}
		operationID := fmt.Sprintf("download-%06d", i)
		sourceRef := fmt.Sprintf("remote-input:%06d", i)
		targetRef := fmt.Sprintf("local-output:%06d", i)
		safety := MCPPlanSafetyAdditive
		if targetSnapshot.exists {
			safety = MCPPlanSafetyDestructive
			output.Summary.ExistingLocalTargets++
		}
		var estimatedBytes *int64
		if item.info != nil && int64(item.info.FileSize) >= 0 {
			size := int64(item.info.FileSize)
			estimatedBytes = &size
		}
		generic.Operations = append(generic.Operations, MCPPlanOperation{
			ID:             operationID,
			Operation:      "download",
			SafetyClass:    safety,
			SourceRef:      sourceRef,
			TargetRef:      targetRef,
			EstimatedBytes: estimatedBytes,
		})
		generic.Preconditions = append(generic.Preconditions,
			MCPPlanPrecondition{OperationID: operationID, Kind: "download_source_snapshot", Ref: sourceRef, Expected: sourceSnapshot},
			MCPPlanPrecondition{OperationID: operationID, Kind: "local_target_snapshot", Ref: targetRef, Expected: targetSnapshot.token},
		)
		fileSize := estimatedBytes
		fileName := ""
		if item.info != nil {
			fileName = item.info.FileName
		}
		output.Items = append(output.Items, MCPTransferPlanItem{
			OperationID:  operationID,
			Index:        i,
			Direction:    "download",
			FileName:     fileName,
			FileSize:     fileSize,
			TargetExists: targetSnapshot.exists,
			SafetyClass:  safety,
		})
		if err := appendKnownTransferBytes(&output.Summary, estimatedBytes); err != nil {
			return MCPTransferPlanOutput{}, err
		}
	}

	plan, err := finalizeMCPPlan(generic)
	if err != nil {
		return MCPTransferPlanOutput{}, err
	}
	output.Plan = plan
	return output, nil
}

func planMCPTransfer(ctx context.Context, ft *FileTools, args PlanTransferArgs) (MCPTransferPlanOutput, error) {
	if ft == nil {
		return MCPTransferPlanOutput{}, fmt.Errorf("file tools are unavailable")
	}
	total := len(args.Uploads) + len(args.Downloads)
	if total == 0 {
		return MCPTransferPlanOutput{}, fmt.Errorf("at least one upload or download is required")
	}
	if total > maxMCPFileBatchItems {
		return MCPTransferPlanOutput{}, fmt.Errorf("transfer plan has %d items; maximum is %d", total, maxMCPFileBatchItems)
	}

	var uploads []mcpPreparedLocalUpload
	var err error
	if len(args.Uploads) > 0 {
		uploads, err = ft.preflightMCPLocalUploadFiles(args.Uploads)
		if err != nil {
			return MCPTransferPlanOutput{}, err
		}
		defer closeMCPPreparedLocalUploads(uploads)
	}

	var downloads []mcpDownloadBatchTransferItem
	if len(args.Downloads) > 0 {
		preflight, err := ft.preflightMCPDownloadFiles(ctx, args.Downloads)
		if err != nil {
			return MCPTransferPlanOutput{}, err
		}
		downloads = preflight.items
	}
	return buildMCPTransferPlan(uploads, downloads, args.MaxChecksumBytes)
}

func redactPlanTransferError(err error, args PlanTransferArgs) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	values := make(map[string]struct{})
	add := func(value string) {
		value = strings.TrimSpace(value)
		if len(value) >= 3 {
			values[value] = struct{}{}
		}
	}
	addLocalPath := func(value string) {
		add(value)
		if absolute, absErr := filepath.Abs(strings.TrimSpace(value)); absErr == nil {
			add(absolute)
		}
	}
	for _, item := range args.Uploads {
		addLocalPath(item.LocalPath)
	}
	for _, item := range args.Downloads {
		addLocalPath(item.LocalPath)
		add(item.PickCode)
		add(item.UserAgent)
	}
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, value := range ordered {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	return text
}

func planTransferCallResult(response MCPTransferPlanOutput) (*mcp.CallToolResult, MCPTransferPlanOutput, error) {
	return mcpTypedJSONResult("plan_transfer", response, response, false)
}

func (ft *FileTools) planTransfer(ctx context.Context, req *mcp.CallToolRequest, args PlanTransferArgs) (*mcp.CallToolResult, MCPTransferPlanOutput, error) {
	response, err := planMCPTransfer(ctx, ft, args)
	if err != nil {
		return toolError("plan_transfer failed: " + redactPlanTransferError(err, args)), MCPTransferPlanOutput{}, nil
	}
	return planTransferCallResult(response)
}
