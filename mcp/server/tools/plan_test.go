package tools

import (
	"strings"
	"testing"
)

func int64Ptr(value int64) *int64 { return &value }

func TestFinalizeMCPPlanDerivesSafetyEstimateAndStableIdentity(t *testing.T) {
	plan := MCPPlan{
		Kind:        " sync ",
		CreatedFrom: " plan_sync ",
		Operations: []MCPPlanOperation{
			{ID: "copy-a", Operation: "copy", SafetyClass: MCPPlanSafetyAdditive, SourceRef: "remote:a", TargetRef: "remote:b", EstimatedBytes: int64Ptr(3)},
			{ID: "delete-old", Operation: "delete", SafetyClass: MCPPlanSafetyDestructive, TargetRef: "remote:old", EstimatedBytes: int64Ptr(0)},
		},
		Dependencies:  []MCPPlanDependency{{OperationID: "delete-old", DependsOn: "copy-a"}},
		Preconditions: []MCPPlanPrecondition{{OperationID: "copy-a", Kind: "sha1", Ref: "remote:a", Expected: "abc"}},
	}

	first, err := finalizeMCPPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := finalizeMCPPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanVersion != MCPPlanVersion || first.Kind != "sync" || first.CreatedFrom != "plan_sync" || first.SafetyClass != MCPPlanSafetyDestructive {
		t.Fatalf("normalized plan metadata = %#v", first)
	}
	if first.EstimatedBytes == nil || *first.EstimatedBytes != 3 {
		t.Fatalf("derived estimated_bytes = %#v", first.EstimatedBytes)
	}
	if !strings.HasPrefix(first.PlanID, "sha256:") || len(first.PlanID) != len("sha256:")+64 || first.PlanID != second.PlanID {
		t.Fatalf("unstable plan identity first=%q second=%q", first.PlanID, second.PlanID)
	}
	if _, err := verifyMCPPlan(first); err != nil {
		t.Fatalf("verify finalized plan: %v", err)
	}
}

func TestVerifyMCPPlanRejectsTampering(t *testing.T) {
	plan, err := finalizeMCPPlan(MCPPlan{
		Kind:        "transfer",
		CreatedFrom: "plan_transfer",
		Operations:  []MCPPlanOperation{{ID: "upload-0", Operation: "upload", SafetyClass: MCPPlanSafetyAdditive, EstimatedBytes: int64Ptr(10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].TargetRef = "changed-target"
	if _, err := verifyMCPPlan(plan); err == nil || !strings.Contains(err.Error(), "plan_id mismatch") {
		t.Fatalf("tampered plan was not rejected: %v", err)
	}
}

func TestFinalizeMCPPlanCanonicalizesDependencyAndPreconditionOrder(t *testing.T) {
	operations := []MCPPlanOperation{
		{ID: "a", Operation: "read", SafetyClass: MCPPlanSafetyReadOnly},
		{ID: "b", Operation: "copy", SafetyClass: MCPPlanSafetyAdditive},
		{ID: "c", Operation: "delete", SafetyClass: MCPPlanSafetyDestructive},
	}
	first, err := finalizeMCPPlan(MCPPlan{
		Kind:        "sync",
		CreatedFrom: "plan_sync",
		Operations:  append([]MCPPlanOperation(nil), operations...),
		Dependencies: []MCPPlanDependency{
			{OperationID: "c", DependsOn: "b"},
			{OperationID: "b", DependsOn: "a"},
		},
		Preconditions: []MCPPlanPrecondition{
			{OperationID: "c", Kind: "exists", Ref: "remote:c", Expected: "true"},
			{OperationID: "a", Kind: "sha1", Ref: "remote:a", Expected: "abc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := finalizeMCPPlan(MCPPlan{
		Kind:        "sync",
		CreatedFrom: "plan_sync",
		Operations:  append([]MCPPlanOperation(nil), operations...),
		Dependencies: []MCPPlanDependency{
			{OperationID: "b", DependsOn: "a"},
			{OperationID: "c", DependsOn: "b"},
		},
		Preconditions: []MCPPlanPrecondition{
			{OperationID: "a", Kind: "sha1", Ref: "remote:a", Expected: "abc"},
			{OperationID: "c", Kind: "exists", Ref: "remote:c", Expected: "true"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanID != second.PlanID {
		t.Fatalf("logical plan order changed plan_id: first=%s second=%s", first.PlanID, second.PlanID)
	}
	if first.Dependencies[0].OperationID != "b" || first.Preconditions[0].OperationID != "a" {
		t.Fatalf("plan fields were not canonicalized: %#v", first)
	}
}

func TestNormalizeMCPPlanRejectsInvalidGraphsAndFields(t *testing.T) {
	base := MCPPlan{
		Kind:        "sync",
		CreatedFrom: "plan_sync",
		Operations: []MCPPlanOperation{
			{ID: "a", Operation: "copy", SafetyClass: MCPPlanSafetyAdditive},
			{ID: "b", Operation: "delete", SafetyClass: MCPPlanSafetyDestructive},
		},
	}
	cases := map[string]MCPPlan{
		"bad-version": func() MCPPlan { p := base; p.PlanVersion = MCPPlanVersion + 1; return p }(),
		"duplicate-op": func() MCPPlan {
			p := base
			p.Operations = append([]MCPPlanOperation(nil), base.Operations...)
			p.Operations[1].ID = "a"
			return p
		}(),
		"bad-safety": func() MCPPlan {
			p := base
			p.Operations = append([]MCPPlanOperation(nil), base.Operations...)
			p.Operations[0].SafetyClass = "unknown"
			return p
		}(),
		"negative-bytes": func() MCPPlan {
			p := base
			p.Operations = append([]MCPPlanOperation(nil), base.Operations...)
			p.Operations[0].EstimatedBytes = int64Ptr(-1)
			return p
		}(),
		"mismatched-total-bytes": func() MCPPlan {
			p := base
			p.Operations = append([]MCPPlanOperation(nil), base.Operations...)
			p.Operations[0].EstimatedBytes = int64Ptr(1)
			p.Operations[1].EstimatedBytes = int64Ptr(2)
			p.EstimatedBytes = int64Ptr(9)
			return p
		}(),
		"unknown-dependency": func() MCPPlan {
			p := base
			p.Dependencies = []MCPPlanDependency{{OperationID: "b", DependsOn: "missing"}}
			return p
		}(),
		"self-dependency": func() MCPPlan {
			p := base
			p.Dependencies = []MCPPlanDependency{{OperationID: "a", DependsOn: "a"}}
			return p
		}(),
		"cycle": func() MCPPlan {
			p := base
			p.Dependencies = []MCPPlanDependency{{OperationID: "a", DependsOn: "b"}, {OperationID: "b", DependsOn: "a"}}
			return p
		}(),
		"bad-precondition": func() MCPPlan {
			p := base
			p.Preconditions = []MCPPlanPrecondition{{OperationID: "missing", Kind: "sha1", Ref: "remote:a"}}
			return p
		}(),
		"duplicate-precondition": func() MCPPlan {
			p := base
			p.Preconditions = []MCPPlanPrecondition{{Kind: "exists", Ref: "remote:a"}, {Kind: "exists", Ref: "remote:a"}}
			return p
		}(),
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := finalizeMCPPlan(plan); err == nil {
				t.Fatal("expected plan validation failure")
			}
		})
	}
}

func TestFinalizeMCPPlanAllowsEmptyNoOpPlanAsReadOnly(t *testing.T) {
	plan, err := finalizeMCPPlan(MCPPlan{Kind: "sync", CreatedFrom: "plan_sync"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SafetyClass != MCPPlanSafetyReadOnly || plan.EstimatedBytes != nil || len(plan.Operations) != 0 {
		t.Fatalf("no-op plan normalization = %#v", plan)
	}
}
