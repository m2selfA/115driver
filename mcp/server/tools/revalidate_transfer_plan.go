package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RevalidateTransferPlanArgs repeats the original plan_transfer inputs and binds
// them to a reviewed MCPPlan v1 identity. Revalidation is read-only and
// stateless: current local/remote state is preflighted and snapshotted again.
type RevalidateTransferPlanArgs struct {
	Uploads          []UploadFromLocalFileItem `json:"uploads,omitempty" jsonschema:"same upload items supplied to plan_transfer"`
	Downloads        []DownloadFileArgs        `json:"downloads,omitempty" jsonschema:"same download items supplied to plan_transfer"`
	MaxChecksumBytes int64                     `json:"max_checksum_bytes,omitempty" jsonschema:"same aggregate local content checksum budget supplied to plan_transfer"`
	ExpectPlanID     string                    `json:"expect_plan_id" jsonschema:"required reviewed MCPPlan v1 plan_id returned by plan_transfer"`
}

// MCPTransferPlanRevalidationOutput reports only whether the reviewed transfer
// plan still describes current state. A mismatch deliberately withholds the new
// plan ID and changed operations so callers must run plan_transfer again.
type MCPTransferPlanRevalidationOutput struct {
	Matches              bool               `json:"matches" jsonschema:"whether fresh replanning produced the reviewed plan_id"`
	GateSatisfied        bool               `json:"gate_satisfied" jsonschema:"true only when the reviewed transfer plan still matches current state"`
	PlanID               string             `json:"plan_id,omitempty" jsonschema:"reviewed plan_id, returned only when it still matches"`
	SafetyClass          MCPPlanSafetyClass `json:"safety_class,omitempty"`
	OperationCount       int                `json:"operation_count,omitempty"`
	ExistingLocalTargets int                `json:"existing_local_targets,omitempty"`
	KnownTransferBytes   int64              `json:"known_transfer_bytes,omitempty"`
	UnknownSizeTransfers int                `json:"unknown_size_transfers,omitempty"`
	ChecksummedFiles     int                `json:"checksummed_files,omitempty"`
	ChecksummedBytes     int64              `json:"checksummed_bytes,omitempty"`
	ErrorCode            string             `json:"error_code,omitempty" jsonschema:"stable revalidation status code"`
	Error                string             `json:"error,omitempty" jsonschema:"sanitized revalidation message that never returns a fresh replacement plan"`
}

func (args RevalidateTransferPlanArgs) planTransferArgs() PlanTransferArgs {
	return PlanTransferArgs{
		Uploads:          args.Uploads,
		Downloads:        args.Downloads,
		MaxChecksumBytes: args.MaxChecksumBytes,
	}
}

func revalidateMCPTransferPlan(ctx context.Context, ft *FileTools, args RevalidateTransferPlanArgs) MCPTransferPlanRevalidationOutput {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return MCPTransferPlanRevalidationOutput{
			ErrorCode: "invalid_expect_plan_id",
			Error:     "expect_plan_id must use sha256:<64 hex> format",
		}
	}
	if expectedPlanID == "" {
		return MCPTransferPlanRevalidationOutput{
			ErrorCode: "expect_plan_id_required",
			Error:     "expect_plan_id is required",
		}
	}
	if ft == nil {
		return MCPTransferPlanRevalidationOutput{
			ErrorCode: "revalidation_failed",
			Error:     "transfer plan revalidation failed; run plan_transfer again",
		}
	}

	fresh, err := planMCPTransfer(ctx, ft, args.planTransferArgs())
	if err != nil {
		// Never reflect paths, pick codes, user agents, newly observed metadata,
		// or a replacement plan through a failed live-state check.
		return MCPTransferPlanRevalidationOutput{
			ErrorCode: "revalidation_failed",
			Error:     "transfer plan revalidation failed; run plan_transfer again",
		}
	}
	if fresh.Plan.PlanID != expectedPlanID {
		return MCPTransferPlanRevalidationOutput{
			ErrorCode: "plan_changed",
			Error:     "transfer plan no longer matches expect_plan_id; run plan_transfer again",
		}
	}

	return MCPTransferPlanRevalidationOutput{
		Matches:              true,
		GateSatisfied:        true,
		PlanID:               expectedPlanID,
		SafetyClass:          fresh.Plan.SafetyClass,
		OperationCount:       len(fresh.Plan.Operations),
		ExistingLocalTargets: fresh.Summary.ExistingLocalTargets,
		KnownTransferBytes:   fresh.Summary.KnownTransferBytes,
		UnknownSizeTransfers: fresh.Summary.UnknownSizeTransfers,
		ChecksummedFiles:     fresh.Summary.ChecksummedFiles,
		ChecksummedBytes:     fresh.Summary.ChecksummedBytes,
	}
}

func revalidateTransferPlanCallResult(response MCPTransferPlanRevalidationOutput) (*mcp.CallToolResult, MCPTransferPlanRevalidationOutput, error) {
	return mcpTypedJSONResult("revalidate_transfer_plan", response, response, false)
}

func (ft *FileTools) revalidateTransferPlan(ctx context.Context, req *mcp.CallToolRequest, args RevalidateTransferPlanArgs) (*mcp.CallToolResult, MCPTransferPlanRevalidationOutput, error) {
	response := revalidateMCPTransferPlan(ctx, ft, args)
	return revalidateTransferPlanCallResult(response)
}
