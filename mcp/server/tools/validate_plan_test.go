package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func validatePlanFixture(t *testing.T) MCPPlan {
	t.Helper()
	one := int64(1)
	two := int64(2)
	plan, err := finalizeMCPPlan(MCPPlan{
		Kind:        "fixture",
		CreatedFrom: "validate-plan-test",
		Operations: []MCPPlanOperation{
			{ID: "a", Operation: "prepare", SafetyClass: MCPPlanSafetyAdditive, EstimatedBytes: &one},
			{ID: "b", Operation: "write", SafetyClass: MCPPlanSafetyDestructive, EstimatedBytes: &two},
		},
		Dependencies: []MCPPlanDependency{{OperationID: "b", DependsOn: "a"}},
		Preconditions: []MCPPlanPrecondition{
			{OperationID: "b", Kind: "target", Ref: "target:b", Expected: "opaque-b"},
			{OperationID: "a", Kind: "source", Ref: "source:a", Expected: "opaque-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestValidateMCPPlanEnvelopeAcceptsCanonicalPlanWithoutReflectingRefs(t *testing.T) {
	plan := validatePlanFixture(t)
	result := validateMCPPlanEnvelope(ValidatePlanArgs{Plan: plan})
	if !result.Valid || !result.Canonical || result.PlanID != plan.PlanID || result.OperationCount != 2 || result.DependencyCount != 1 || result.PreconditionCount != 2 || result.SafetyClass != MCPPlanSafetyDestructive || result.EstimatedBytes == nil || *result.EstimatedBytes != 3 {
		t.Fatalf("validate_plan canonical result = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"source:a", "target:b", "opaque-a", "opaque-b", "source_ref", "target_ref", "expected"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("validate_plan reflected plan detail %q: %s", forbidden, text)
		}
	}
}

func TestValidateMCPPlanEnvelopeRejectsTampering(t *testing.T) {
	plan := validatePlanFixture(t)
	plan.Operations[1].Operation = "tampered"
	result := validateMCPPlanEnvelope(ValidatePlanArgs{Plan: plan})
	if result.Valid || result.ErrorCode != "plan_id_mismatch" || result.Error != "plan content does not match plan_id" || result.PlanID != "" {
		t.Fatalf("tampered plan validation = %#v", result)
	}
}

func TestValidateMCPPlanEnvelopeAcceptsEquivalentNonCanonicalForm(t *testing.T) {
	plan := validatePlanFixture(t)
	plan.Kind = "  " + plan.Kind + "  "
	result := validateMCPPlanEnvelope(ValidatePlanArgs{Plan: plan})
	if !result.Valid || result.Canonical || result.PlanID != strings.TrimSpace(plan.PlanID) || result.Kind != "fixture" {
		t.Fatalf("equivalent non-canonical plan validation = %#v", result)
	}
}

func TestValidateMCPPlanEnvelopeNeverReflectsInvalidRefsOrExpectedValues(t *testing.T) {
	const secretRef = "secret-ref-value"
	const secretExpected = "secret-expected-value"
	plan := validatePlanFixture(t)
	plan.PlanID = strings.Repeat("x", len(plan.PlanID))
	plan.Preconditions = append(plan.Preconditions, MCPPlanPrecondition{
		OperationID: "missing-secret-operation",
		Kind:        "secret-kind",
		Ref:         secretRef,
		Expected:    secretExpected,
	})
	result := validateMCPPlanEnvelope(ValidatePlanArgs{Plan: plan})
	if result.Valid || result.ErrorCode != "invalid_plan" || result.Error != "plan structure is invalid" {
		t.Fatalf("invalid secret-bearing plan validation = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{secretRef, secretExpected, "missing-secret-operation", "secret-kind"} {
		if strings.Contains(text, secret) {
			t.Fatalf("validate_plan reflected invalid-plan secret %q: %s", secret, text)
		}
	}
}

func TestValidatePlanCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPPlanValidationResult{Valid: true, Canonical: true, PlanID: "sha256:test", PlanVersion: 1, Kind: "transfer", SafetyClass: MCPPlanSafetyAdditive, OperationCount: 1}
	result, output, err := validatePlanCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("validate_plan result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPPlanValidationResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != output {
		t.Fatalf("validate_plan text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}
