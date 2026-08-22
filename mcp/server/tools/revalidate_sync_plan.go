package tools

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RevalidateSyncPlanArgs repeats the original plan_sync inputs and binds them to
// a reviewed MCPPlan v1 identity. The server remains stateless: every call
// rebuilds the sync plan from current local and remote state.
type RevalidateSyncPlanArgs struct {
	LocalPath        string `json:"local_path" jsonschema:"same existing local directory supplied to plan_sync"`
	RemotePath       string `json:"remote_path" jsonschema:"same existing remote 115 directory path supplied to plan_sync"`
	Direction        string `json:"direction,omitempty" jsonschema:"same sync direction supplied to plan_sync: both, upload, or download"`
	ConflictPolicy   string `json:"conflict_policy,omitempty" jsonschema:"same conflict policy supplied to plan_sync: error, prefer-local, or prefer-remote"`
	DeleteExtraneous bool   `json:"delete,omitempty" jsonschema:"same mirror-delete choice supplied to plan_sync"`
	MaxNodes         int    `json:"max_nodes,omitempty" jsonschema:"same aggregate local plus remote descendant budget supplied to plan_sync"`
	MaxChecksumBytes int64  `json:"max_checksum_bytes,omitempty" jsonschema:"same local checksum budget supplied to plan_sync"`
	ExpectPlanID     string `json:"expect_plan_id" jsonschema:"required reviewed MCPPlan v1 plan_id returned by plan_sync"`
}

// MCPSyncPlanRevalidationOutput reports only whether the reviewed plan still
// describes current state. A mismatch deliberately does not return the new plan
// or its identity; callers must run plan_sync again to review changed actions.
type MCPSyncPlanRevalidationOutput struct {
	Matches                  bool               `json:"matches" jsonschema:"whether fresh replanning produced the reviewed plan_id"`
	Ready                    bool               `json:"ready" jsonschema:"whether the matching fresh sync plan has no unresolved conflicts"`
	GateSatisfied            bool               `json:"gate_satisfied" jsonschema:"true only when the reviewed plan matches current state and is ready"`
	PlanID                   string             `json:"plan_id,omitempty" jsonschema:"reviewed plan_id, returned only when it still matches"`
	SafetyClass              MCPPlanSafetyClass `json:"safety_class,omitempty"`
	OperationCount           int                `json:"operation_count,omitempty"`
	DestructiveActions       int                `json:"destructive_actions,omitempty"`
	RequiresAllowDestructive bool               `json:"requires_allow_destructive,omitempty"`
	ChecksummedFiles         int                `json:"checksummed_files,omitempty"`
	ChecksummedBytes         int64              `json:"checksummed_bytes,omitempty"`
	ErrorCode                string             `json:"error_code,omitempty" jsonschema:"stable revalidation status code"`
	Error                    string             `json:"error,omitempty" jsonschema:"sanitized revalidation message that never returns a fresh replacement plan"`
}

func (args RevalidateSyncPlanArgs) planSyncArgs() PlanSyncArgs {
	return PlanSyncArgs{
		LocalPath:        args.LocalPath,
		RemotePath:       args.RemotePath,
		Direction:        args.Direction,
		ConflictPolicy:   args.ConflictPolicy,
		DeleteExtraneous: args.DeleteExtraneous,
		MaxNodes:         args.MaxNodes,
		MaxChecksumBytes: args.MaxChecksumBytes,
	}
}

func addMCPSyncRedactionValue(values map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	if len(value) >= 3 {
		values[value] = struct{}{}
	}
}

func addMCPSyncLocalPathRedactionValues(values map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	addMCPSyncRedactionValue(values, value)
	if value == "" {
		return
	}
	addMCPSyncRedactionValue(values, filepath.ToSlash(value))
	if absolute, err := filepath.Abs(value); err == nil {
		addMCPSyncRedactionValue(values, absolute)
		addMCPSyncRedactionValue(values, filepath.ToSlash(absolute))
		if canonical, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
			addMCPSyncRedactionValue(values, canonical)
			addMCPSyncRedactionValue(values, filepath.ToSlash(canonical))
		}
	}
}

func redactMCPSyncValues(text string, values map[string]struct{}, marker string) string {
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	for _, value := range ordered {
		text = strings.ReplaceAll(text, value, marker)
	}
	return text
}

func redactMCPSyncPlanError(err error, localRoot string, args PlanSyncArgs) string {
	if err == nil {
		return ""
	}
	values := make(map[string]struct{})
	addMCPSyncLocalPathRedactionValues(values, localRoot)
	addMCPSyncLocalPathRedactionValues(values, args.LocalPath)
	return redactMCPSyncValues(err.Error(), values, "[REDACTED_LOCAL_PATH]")
}

func revalidateMCPSyncPlan(ctx context.Context, client mcpSyncPlanClient, localRoot string, args RevalidateSyncPlanArgs) MCPSyncPlanRevalidationOutput {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return MCPSyncPlanRevalidationOutput{
			ErrorCode: "invalid_expect_plan_id",
			Error:     "expect_plan_id must use sha256:<64 hex> format",
		}
	}
	if expectedPlanID == "" {
		return MCPSyncPlanRevalidationOutput{
			ErrorCode: "expect_plan_id_required",
			Error:     "expect_plan_id is required",
		}
	}

	fresh, err := planMCPSync(ctx, client, localRoot, args.planSyncArgs())
	if err != nil {
		// Do not reflect a changed path, object identity, digest, or newly
		// discovered plan shape from a failed live-state check.
		return MCPSyncPlanRevalidationOutput{
			ErrorCode: "revalidation_failed",
			Error:     "sync plan revalidation failed; run plan_sync again",
		}
	}
	if fresh.Plan.PlanID != expectedPlanID {
		return MCPSyncPlanRevalidationOutput{
			ErrorCode: "plan_changed",
			Error:     "sync plan no longer matches expect_plan_id; run plan_sync again",
		}
	}

	return MCPSyncPlanRevalidationOutput{
		Matches:                  true,
		Ready:                    fresh.Summary.Ready,
		GateSatisfied:            fresh.Summary.Ready,
		PlanID:                   expectedPlanID,
		SafetyClass:              fresh.Plan.SafetyClass,
		OperationCount:           len(fresh.Plan.Operations),
		DestructiveActions:       fresh.Summary.DestructiveActions,
		RequiresAllowDestructive: fresh.Summary.RequiresAllowDestructive,
		ChecksummedFiles:         fresh.Summary.ChecksummedFiles,
		ChecksummedBytes:         fresh.Summary.ChecksummedBytes,
	}
}

func revalidateSyncPlanCallResult(response MCPSyncPlanRevalidationOutput) (*mcp.CallToolResult, MCPSyncPlanRevalidationOutput, error) {
	return mcpTypedJSONResult("revalidate_sync_plan", response, response, false)
}

func (ft *FileTools) revalidateSyncPlan(ctx context.Context, req *mcp.CallToolRequest, args RevalidateSyncPlanArgs) (*mcp.CallToolResult, MCPSyncPlanRevalidationOutput, error) {
	response := revalidateMCPSyncPlan(ctx, ft.client, ft.localRoot, args)
	return revalidateSyncPlanCallResult(response)
}
