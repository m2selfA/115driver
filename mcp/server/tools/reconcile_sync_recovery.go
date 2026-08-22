package tools

import (
	"context"
	"errors"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReconcileSyncRecoveryArgs struct {
	PlanID            string `json:"plan_id" jsonschema:"reviewed sha256:<64hex> plan_id returned by plan_sync"`
	ExpectDiagnosisID string `json:"expect_diagnosis_id" jsonschema:"required sha256:<64hex> diagnosis_id returned by diagnose_sync_recovery for the exact reviewed evidence"`
}

type ReconcileSyncRecoveryOutput struct {
	Applied            bool   `json:"applied"`
	PlanID             string `json:"plan_id,omitempty"`
	DiagnosisID        string `json:"diagnosis_id,omitempty"`
	Checked            int    `json:"checked"`
	Completed          int    `json:"completed"`
	RetryFull          int    `json:"retry_full"`
	WinnerOnly         int    `json:"winner_only"`
	PendingObservation int    `json:"pending_observation"`
	JournalState       string `json:"journal_state,omitempty"`
	JournalStatus      string `json:"journal_status,omitempty"`
	ResumeCandidate    bool   `json:"resume_candidate" jsonschema:"true when journal control state has no remaining recovery/reconcile gate; execute_sync_plan still reruns full state preflight"`
	ErrorCode          string `json:"error_code,omitempty"`
	Error              string `json:"error,omitempty" jsonschema:"sanitized recovery reconciliation error"`
}

func mcpSyncRecoveryReconcileCallResult(output ReconcileSyncRecoveryOutput) (*mcp.CallToolResult, ReconcileSyncRecoveryOutput, error) {
	return mcpTypedJSONResult("reconcile_sync_recovery", output, output, output.ErrorCode != "")
}

func reconcileSyncRecoveryError(planID, diagnosisID, code, message string) (*mcp.CallToolResult, ReconcileSyncRecoveryOutput, error) {
	return mcpSyncRecoveryReconcileCallResult(ReconcileSyncRecoveryOutput{
		PlanID: planID, DiagnosisID: diagnosisID, ErrorCode: code, Error: message,
	})
}

// reconcileSyncRecovery records explicitly reviewed recovery evidence in the
// persistent journal. It never mutates local files or 115 objects. A caller must
// first obtain diagnosis_id from diagnose_sync_recovery; this function acquires
// the shared journal lock, re-collects evidence, and applies it only when the
// content-addressed diagnosis still matches exactly.
func (ft *FileTools) reconcileSyncRecovery(ctx context.Context, req *mcp.CallToolRequest, args ReconcileSyncRecoveryArgs) (*mcp.CallToolResult, ReconcileSyncRecoveryOutput, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(args.PlanID)
	if err != nil || reviewedPlanID == "" {
		return reconcileSyncRecoveryError(reviewedPlanID, "", "invalid_plan_id", "plan_id must be a reviewed sha256:<64hex> plan_sync ID")
	}
	expectedDiagnosisID, err := normalizeMCPExpectedPlanID(args.ExpectDiagnosisID)
	if err != nil || expectedDiagnosisID == "" {
		return reconcileSyncRecoveryError(reviewedPlanID, "", "invalid_diagnosis_id", "expect_diagnosis_id must be the sha256:<64hex> token returned by diagnose_sync_recovery")
	}

	lookup := ft.lookupMCPSyncJournal(ctx, reviewedPlanID)
	if !lookup.found() {
		code := lookup.ErrorCode
		message := lookup.Error
		if code == "" {
			code = "journal_not_found"
			message = "no persistent sync journal was found for the reviewed plan"
		}
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, code, message)
	}

	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "journal_unavailable", "persistent sync journal store is unavailable")
	}
	rawPlanID := lookup.Record.Journal.PlanID
	handle, err := store.OpenCurrent(rawPlanID)
	if errors.Is(err, transfer.ErrSessionLocked) {
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "journal_in_use", "the sync execution is currently in use; recovery evidence cannot be reconciled")
	}
	if err != nil {
		switch {
		case errors.Is(err, syncjournalpkg.ErrMigrationRequired):
			return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "journal_migration_required", "the sync journal uses an older schema; migrate it with the 115driver CLI before recovery")
		case errors.Is(err, syncjournalpkg.ErrNotFound):
			return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "journal_not_found", "the reviewed sync journal no longer exists")
		default:
			return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "journal_read_failed", "persistent sync journal could not be opened safely")
		}
	}
	defer handle.Close()

	snapshot := handle.Snapshot()
	if snapshot.PlanID != rawPlanID || snapshot.State == syncjournalpkg.StatusCompleted {
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "recovery_not_required", "the persistent sync journal does not require recovery reconciliation")
	}
	if !syncjournalpkg.RecoveryRequired(snapshot) && !syncjournalpkg.ReconciliationRequired(snapshot) {
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "recovery_not_required", "the persistent sync journal has no unresolved recovery state")
	}

	diagnosis, evidence := ft.diagnoseSyncRecoveryJournal(ctx, reviewedPlanID, snapshot)
	if diagnosis.ErrorCode != "" || !diagnosis.EvidenceComplete {
		return reconcileSyncRecoveryError(reviewedPlanID, expectedDiagnosisID, "evidence_failed", "recovery evidence could not be classified safely")
	}
	if diagnosis.DiagnosisID == "" || diagnosis.DiagnosisID != expectedDiagnosisID {
		// Never return the freshly computed token on mismatch: doing so would let
		// a caller bypass explicit re-review by feeding it straight back here.
		return reconcileSyncRecoveryError(reviewedPlanID, "", "diagnosis_changed", "recovery evidence changed after review; run diagnose_sync_recovery again")
	}
	if diagnosis.PendingObservation > 0 {
		return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "postcondition_pending", "one or more completed mutations are not yet observable; diagnose the journal again after the target state becomes visible")
	}
	if !diagnosis.Resolvable || diagnosis.Checked == 0 || diagnosis.Ambiguous > 0 || diagnosis.Errors > 0 {
		return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "recovery_ambiguous", "recovery evidence remains ambiguous and cannot be applied")
	}

	// Older current-v2 journals may predate the private external-plan alias. Once
	// the exact reviewed diagnosis matches under the journal lock, establish the
	// same private bridge used by normal MCP execution so residual resume can use
	// the original public plan_id without exposing the internal journal ID.
	if _, err := handle.BindReviewAlias(reviewedPlanID); err != nil {
		switch {
		case errors.Is(err, syncjournalpkg.ErrReviewAliasInUse):
			return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "journal_in_use", "the reviewed sync journal binding is currently in use")
		case errors.Is(err, syncjournalpkg.ErrReviewAliasConflict):
			return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "journal_alias_conflict", "the reviewed plan is already bound to a different persistent sync journal")
		default:
			return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "journal_alias_failed", "the reviewed sync journal binding could not be persisted safely")
		}
	}

	now := time.Now().UTC()
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		// OpenCurrent holds the cross-process journal lock, but compare immutable
		// and review-token inputs again so any unexpected in-process drift fails
		// closed before a control-plane state change.
		if journal.PlanID != snapshot.PlanID || len(journal.Items) != len(snapshot.Items) || !journal.UpdatedAt.Equal(snapshot.UpdatedAt) {
			return errors.New("sync journal changed during recovery reconciliation")
		}
		for _, observed := range evidence {
			if observed.Err != nil || observed.Index < 0 || observed.Index >= len(journal.Items) {
				return errors.New("recovery evidence is incomplete")
			}
			before := snapshot.Items[observed.Index]
			current := journal.Items[observed.Index]
			if current.State != before.State || current.Phase != before.Phase || current.Action != before.Action || current.Index != before.Index {
				return errors.New("sync journal recovery item changed during reconciliation")
			}
			if observed.Destructive {
				if err := syncjournalpkg.ApplyDestructiveDecision(journal, observed.Index, syncjournalpkg.DestructiveDecision(observed.Decision), observed.Post, now); err != nil {
					return err
				}
				continue
			}
			if observed.Decision != string(syncjournalpkg.DestructiveCompleted) || observed.Post == nil {
				return errors.New("non-destructive recovery evidence is not a verified completion")
			}
			if err := syncjournalpkg.SucceedItem(journal, observed.Index, journal.Plan.Items[observed.Index], observed.Post, now); err != nil {
				return err
			}
		}
		journal.State = syncjournalpkg.StatusActive
		journal.LastError = ""
		journal.CompletedAt = nil
		return nil
	}); err != nil {
		return reconcileSyncRecoveryError(reviewedPlanID, diagnosis.DiagnosisID, "reconciliation_failed", "recovery evidence could not be applied atomically")
	}

	updated := handle.Snapshot()
	resumeCandidate := !syncjournalpkg.RecoveryRequired(updated) && !syncjournalpkg.ReconciliationRequired(updated)
	return mcpSyncRecoveryReconcileCallResult(ReconcileSyncRecoveryOutput{
		Applied: true, PlanID: reviewedPlanID, DiagnosisID: diagnosis.DiagnosisID,
		Checked: diagnosis.Checked, Completed: diagnosis.Completed, RetryFull: diagnosis.RetryFull, WinnerOnly: diagnosis.WinnerOnly, PendingObservation: diagnosis.PendingObservation,
		JournalState: updated.State, JournalStatus: syncjournalpkg.EffectiveStatus(updated), ResumeCandidate: resumeCandidate,
	})
}
