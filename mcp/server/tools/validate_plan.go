package tools

import (
	"context"
	"reflect"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ValidatePlanArgs accepts an MCPPlan v1 produced by any planner. Validation is
// purely structural/content-addressed: it performs no remote or local state I/O.
type ValidatePlanArgs struct {
	Plan MCPPlan `json:"plan" jsonschema:"MCPPlan v1 envelope to validate"`
}

// MCPPlanValidationResult deliberately reports only aggregate safe metadata. It
// never reflects source/target refs or opaque precondition values back to MCP.
type MCPPlanValidationResult struct {
	Valid             bool               `json:"valid" jsonschema:"whether plan structure and content-addressed identity are valid"`
	Canonical         bool               `json:"canonical" jsonschema:"whether the supplied plan already matches canonical normalized ordering and fields"`
	PlanID            string             `json:"plan_id,omitempty"`
	PlanVersion       int                `json:"plan_version,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	CreatedFrom       string             `json:"created_from,omitempty"`
	SafetyClass       MCPPlanSafetyClass `json:"safety_class,omitempty"`
	OperationCount    int                `json:"operation_count,omitempty"`
	DependencyCount   int                `json:"dependency_count,omitempty"`
	PreconditionCount int                `json:"precondition_count,omitempty"`
	EstimatedBytes    *int64             `json:"estimated_bytes,omitempty"`
	ErrorCode         string             `json:"error_code,omitempty" jsonschema:"stable validation failure category"`
	Error             string             `json:"error,omitempty" jsonschema:"sanitized validation failure without echoing plan refs or precondition values"`
}

func safeMCPPlanValidationFailure(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "plan_id is required"):
		return "plan_id_required", "plan_id is required"
	case strings.Contains(message, "plan_id mismatch"):
		return "plan_id_mismatch", "plan content does not match plan_id"
	case strings.Contains(message, "unsupported plan_version"):
		return "unsupported_plan_version", "plan_version is not supported"
	default:
		return "invalid_plan", "plan structure is invalid"
	}
}

func validateMCPPlanEnvelope(args ValidatePlanArgs) MCPPlanValidationResult {
	normalized, err := verifyMCPPlan(args.Plan)
	if err != nil {
		code, message := safeMCPPlanValidationFailure(err)
		return MCPPlanValidationResult{Valid: false, ErrorCode: code, Error: message}
	}
	return MCPPlanValidationResult{
		Valid:             true,
		Canonical:         reflect.DeepEqual(args.Plan, normalized),
		PlanID:            normalized.PlanID,
		PlanVersion:       normalized.PlanVersion,
		Kind:              normalized.Kind,
		CreatedFrom:       normalized.CreatedFrom,
		SafetyClass:       normalized.SafetyClass,
		OperationCount:    len(normalized.Operations),
		DependencyCount:   len(normalized.Dependencies),
		PreconditionCount: len(normalized.Preconditions),
		EstimatedBytes:    normalized.EstimatedBytes,
	}
}

func validatePlanCallResult(response MCPPlanValidationResult) (*mcp.CallToolResult, MCPPlanValidationResult, error) {
	return mcpTypedJSONResult("validate_plan", response, response, false)
}

func (ft *FileTools) validatePlan(ctx context.Context, req *mcp.CallToolRequest, args ValidatePlanArgs) (*mcp.CallToolResult, MCPPlanValidationResult, error) {
	return validatePlanCallResult(validateMCPPlanEnvelope(args))
}
