package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const MCPPlanVersion = 1

type MCPPlanSafetyClass string

const (
	MCPPlanSafetyReadOnly    MCPPlanSafetyClass = "read_only"
	MCPPlanSafetyAdditive    MCPPlanSafetyClass = "additive"
	MCPPlanSafetyDestructive MCPPlanSafetyClass = "destructive"
)

// MCPPlanOperation is the reusable operation record for future transfer/sync
// planners. Refs are intentionally opaque, safe planner-level identities rather
// than credentials, signed URLs, or transport endpoints.
type MCPPlanOperation struct {
	ID             string             `json:"id" jsonschema:"stable operation ID unique within the plan"`
	Operation      string             `json:"operation" jsonschema:"planned operation kind"`
	SafetyClass    MCPPlanSafetyClass `json:"safety_class" jsonschema:"read_only, additive, or destructive"`
	SourceRef      string             `json:"source_ref,omitempty" jsonschema:"safe planner-level source identity"`
	TargetRef      string             `json:"target_ref,omitempty" jsonschema:"safe planner-level target identity"`
	EstimatedBytes *int64             `json:"estimated_bytes,omitempty" jsonschema:"estimated bytes for this operation when known"`
}

// MCPPlanDependency is one directed DAG edge: operation_id may execute only
// after depends_on has completed successfully.
type MCPPlanDependency struct {
	OperationID string `json:"operation_id" jsonschema:"dependent operation ID"`
	DependsOn   string `json:"depends_on" jsonschema:"prerequisite operation ID"`
}

// MCPPlanPrecondition records state that must still be true before execution.
// Expected is deliberately an opaque planner-defined value so later planners can
// bind file IDs, digests, sizes, or snapshot tokens without widening this base.
type MCPPlanPrecondition struct {
	OperationID string `json:"operation_id,omitempty" jsonschema:"optional operation ID this precondition belongs to; empty means plan-wide"`
	Kind        string `json:"kind" jsonschema:"precondition kind"`
	Ref         string `json:"ref" jsonschema:"safe object or snapshot identity being checked"`
	Expected    string `json:"expected,omitempty" jsonschema:"opaque expected value or digest"`
}

// MCPPlan is the versioned, content-addressed envelope shared by future MCP
// planning tools. PlanID is SHA-256 over the normalized plan with plan_id empty,
// binding kind, source, operations, dependencies, preconditions, estimates, and
// safety classification against stale/tampered replay.
type MCPPlan struct {
	PlanVersion    int                   `json:"plan_version" jsonschema:"MCP plan schema version"`
	PlanID         string                `json:"plan_id" jsonschema:"content-addressed sha256 plan identity"`
	Kind           string                `json:"kind" jsonschema:"planner kind such as transfer or sync"`
	CreatedFrom    string                `json:"created_from" jsonschema:"MCP tool or planner that produced this plan"`
	SafetyClass    MCPPlanSafetyClass    `json:"safety_class" jsonschema:"maximum safety class across all operations"`
	EstimatedBytes *int64                `json:"estimated_bytes,omitempty" jsonschema:"aggregate estimated bytes when all operation estimates are known"`
	Operations     []MCPPlanOperation    `json:"operations" jsonschema:"planned operations in deterministic order"`
	Dependencies   []MCPPlanDependency   `json:"dependencies,omitempty" jsonschema:"validated acyclic dependency edges"`
	Preconditions  []MCPPlanPrecondition `json:"preconditions,omitempty" jsonschema:"state checks required before execution"`
}

func mcpPlanSafetyRank(class MCPPlanSafetyClass) (int, error) {
	switch class {
	case MCPPlanSafetyReadOnly:
		return 0, nil
	case MCPPlanSafetyAdditive:
		return 1, nil
	case MCPPlanSafetyDestructive:
		return 2, nil
	default:
		return 0, fmt.Errorf("unsupported plan safety_class %q", class)
	}
}

func normalizeMCPPlan(plan MCPPlan) (MCPPlan, error) {
	if plan.PlanVersion == 0 {
		plan.PlanVersion = MCPPlanVersion
	}
	if plan.PlanVersion != MCPPlanVersion {
		return MCPPlan{}, fmt.Errorf("unsupported plan_version %d; expected %d", plan.PlanVersion, MCPPlanVersion)
	}
	plan.Kind = strings.TrimSpace(plan.Kind)
	plan.CreatedFrom = strings.TrimSpace(plan.CreatedFrom)
	if plan.Kind == "" {
		return MCPPlan{}, fmt.Errorf("plan kind is required")
	}
	if plan.CreatedFrom == "" {
		return MCPPlan{}, fmt.Errorf("created_from is required")
	}
	if plan.EstimatedBytes != nil && *plan.EstimatedBytes < 0 {
		return MCPPlan{}, fmt.Errorf("estimated_bytes must not be negative")
	}

	operationIDs := make(map[string]int, len(plan.Operations))
	maxSafetyRank := 0
	allEstimated := len(plan.Operations) > 0
	var estimatedTotal int64
	for i := range plan.Operations {
		op := &plan.Operations[i]
		op.ID = strings.TrimSpace(op.ID)
		op.Operation = strings.TrimSpace(op.Operation)
		op.SourceRef = strings.TrimSpace(op.SourceRef)
		op.TargetRef = strings.TrimSpace(op.TargetRef)
		if op.ID == "" {
			return MCPPlan{}, fmt.Errorf("operation %d has an empty id", i)
		}
		if previous, exists := operationIDs[op.ID]; exists {
			return MCPPlan{}, fmt.Errorf("operations %d and %d use duplicate id %q", previous, i, op.ID)
		}
		operationIDs[op.ID] = i
		if op.Operation == "" {
			return MCPPlan{}, fmt.Errorf("operation %q has an empty operation kind", op.ID)
		}
		rank, err := mcpPlanSafetyRank(op.SafetyClass)
		if err != nil {
			return MCPPlan{}, fmt.Errorf("operation %q: %w", op.ID, err)
		}
		if rank > maxSafetyRank {
			maxSafetyRank = rank
		}
		if op.EstimatedBytes == nil {
			allEstimated = false
		} else if *op.EstimatedBytes < 0 {
			return MCPPlan{}, fmt.Errorf("operation %q estimated_bytes must not be negative", op.ID)
		} else {
			if estimatedTotal > int64(^uint64(0)>>1)-*op.EstimatedBytes {
				return MCPPlan{}, fmt.Errorf("operation byte estimates overflow int64")
			}
			estimatedTotal += *op.EstimatedBytes
		}
	}

	if len(plan.Operations) == 0 {
		plan.SafetyClass = MCPPlanSafetyReadOnly
	} else {
		switch maxSafetyRank {
		case 0:
			plan.SafetyClass = MCPPlanSafetyReadOnly
		case 1:
			plan.SafetyClass = MCPPlanSafetyAdditive
		case 2:
			plan.SafetyClass = MCPPlanSafetyDestructive
		}
	}
	if allEstimated {
		if plan.EstimatedBytes == nil {
			total := estimatedTotal
			plan.EstimatedBytes = &total
		} else if *plan.EstimatedBytes != estimatedTotal {
			return MCPPlan{}, fmt.Errorf("estimated_bytes %d does not match operation total %d", *plan.EstimatedBytes, estimatedTotal)
		}
	}

	seenDependencies := make(map[string]struct{}, len(plan.Dependencies))
	graph := make(map[string][]string, len(operationIDs))
	for i := range plan.Dependencies {
		dep := &plan.Dependencies[i]
		dep.OperationID = strings.TrimSpace(dep.OperationID)
		dep.DependsOn = strings.TrimSpace(dep.DependsOn)
		if _, ok := operationIDs[dep.OperationID]; !ok {
			return MCPPlan{}, fmt.Errorf("dependency %d references unknown operation_id %q", i, dep.OperationID)
		}
		if _, ok := operationIDs[dep.DependsOn]; !ok {
			return MCPPlan{}, fmt.Errorf("dependency %d references unknown depends_on %q", i, dep.DependsOn)
		}
		if dep.OperationID == dep.DependsOn {
			return MCPPlan{}, fmt.Errorf("operation %q cannot depend on itself", dep.OperationID)
		}
		key := dep.OperationID + "\x00" + dep.DependsOn
		if _, exists := seenDependencies[key]; exists {
			return MCPPlan{}, fmt.Errorf("duplicate dependency %q -> %q", dep.OperationID, dep.DependsOn)
		}
		seenDependencies[key] = struct{}{}
		graph[dep.OperationID] = append(graph[dep.OperationID], dep.DependsOn)
	}

	sort.Slice(plan.Dependencies, func(i, j int) bool {
		if plan.Dependencies[i].OperationID != plan.Dependencies[j].OperationID {
			return plan.Dependencies[i].OperationID < plan.Dependencies[j].OperationID
		}
		return plan.Dependencies[i].DependsOn < plan.Dependencies[j].DependsOn
	})

	state := make(map[string]uint8, len(operationIDs))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("plan dependencies contain a cycle through %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, dependency := range graph[id] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range operationIDs {
		if err := visit(id); err != nil {
			return MCPPlan{}, err
		}
	}

	seenPreconditions := make(map[string]struct{}, len(plan.Preconditions))
	for i := range plan.Preconditions {
		precondition := &plan.Preconditions[i]
		precondition.OperationID = strings.TrimSpace(precondition.OperationID)
		precondition.Kind = strings.TrimSpace(precondition.Kind)
		precondition.Ref = strings.TrimSpace(precondition.Ref)
		precondition.Expected = strings.TrimSpace(precondition.Expected)
		if precondition.Kind == "" || precondition.Ref == "" {
			return MCPPlan{}, fmt.Errorf("precondition %d requires non-empty kind and ref", i)
		}
		if precondition.OperationID != "" {
			if _, ok := operationIDs[precondition.OperationID]; !ok {
				return MCPPlan{}, fmt.Errorf("precondition %d references unknown operation_id %q", i, precondition.OperationID)
			}
		}
		key := precondition.OperationID + "\x00" + precondition.Kind + "\x00" + precondition.Ref + "\x00" + precondition.Expected
		if _, exists := seenPreconditions[key]; exists {
			return MCPPlan{}, fmt.Errorf("duplicate precondition %d for ref %q", i, precondition.Ref)
		}
		seenPreconditions[key] = struct{}{}
	}
	sort.Slice(plan.Preconditions, func(i, j int) bool {
		left := plan.Preconditions[i]
		right := plan.Preconditions[j]
		if left.OperationID != right.OperationID {
			return left.OperationID < right.OperationID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Ref != right.Ref {
			return left.Ref < right.Ref
		}
		return left.Expected < right.Expected
	})
	return plan, nil
}

func computeMCPPlanID(plan MCPPlan) (string, error) {
	plan.PlanID = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func finalizeMCPPlan(plan MCPPlan) (MCPPlan, error) {
	normalized, err := normalizeMCPPlan(plan)
	if err != nil {
		return MCPPlan{}, err
	}
	planID, err := computeMCPPlanID(normalized)
	if err != nil {
		return MCPPlan{}, fmt.Errorf("serialize plan identity: %w", err)
	}
	normalized.PlanID = planID
	return normalized, nil
}

func verifyMCPPlan(plan MCPPlan) (MCPPlan, error) {
	providedID := strings.TrimSpace(plan.PlanID)
	if providedID == "" {
		return MCPPlan{}, fmt.Errorf("plan_id is required")
	}
	normalized, err := finalizeMCPPlan(plan)
	if err != nil {
		return MCPPlan{}, err
	}
	if providedID != normalized.PlanID {
		return MCPPlan{}, fmt.Errorf("plan_id mismatch: plan content changed after planning")
	}
	return normalized, nil
}
