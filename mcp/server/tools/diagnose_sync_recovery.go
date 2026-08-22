package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"strings"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type DiagnoseSyncRecoveryArgs struct {
	PlanID string `json:"plan_id" jsonschema:"reviewed sha256:<64hex> plan_id returned by plan_sync"`
}

const maxMCPSyncRecoveryChecksumBytes = maxMCPSyncPlanChecksumBytes

var errMCPSyncRecoveryChecksumBudgetExceeded = errors.New("sync recovery checksum budget exceeded")

type mcpSyncRecoveryChecksumBudget struct {
	limit int64
	used  int64
}

func newMCPSyncRecoveryChecksumBudget() *mcpSyncRecoveryChecksumBudget {
	return &mcpSyncRecoveryChecksumBudget{limit: maxMCPSyncRecoveryChecksumBytes}
}

func (budget *mcpSyncRecoveryChecksumBudget) consume(size int64) error {
	if budget == nil || size <= 0 {
		return nil
	}
	if size > budget.limit || budget.used > budget.limit-size {
		return errMCPSyncRecoveryChecksumBudgetExceeded
	}
	budget.used += size
	return nil
}

type MCPSyncRecoveryItem struct {
	RelativePath string `json:"relative_path"`
	Action       string `json:"action"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Phase        string `json:"phase"`
	Decision     string `json:"decision" jsonschema:"recovery evidence decision: completed, retry-full, winner-only, pending-observation, ambiguous, or error"`
	HasError     bool   `json:"has_error"`
}

type DiagnoseSyncRecoveryOutput struct {
	Found               bool                  `json:"found"`
	PlanID              string                `json:"plan_id,omitempty"`
	DiagnosisID         string                `json:"diagnosis_id,omitempty" jsonschema:"content-addressed sha256 review token for the current destructive evidence"`
	JournalState        string                `json:"journal_state,omitempty"`
	JournalStatus       string                `json:"journal_status,omitempty"`
	InUse               bool                  `json:"in_use"`
	ReconcileRequired   bool                  `json:"reconcile_required"`
	EvidenceComplete    bool                  `json:"evidence_complete"`
	Resolvable          bool                  `json:"resolvable" jsonschema:"true only when every reconciliation-gated item has a completed/retry-full/winner-only evidence decision; this is not resume_ready"`
	Checked             int                   `json:"checked"`
	Completed           int                   `json:"completed"`
	RetryFull           int                   `json:"retry_full"`
	WinnerOnly          int                   `json:"winner_only"`
	PendingObservation  int                   `json:"pending_observation"`
	Ambiguous           int                   `json:"ambiguous"`
	ChecksumBudgetBytes int64                 `json:"checksum_budget_bytes" jsonschema:"fixed maximum local bytes that this diagnosis pass may hash"`
	ChecksummedBytes    int64                 `json:"checksummed_bytes" jsonschema:"local bytes actually hashed while collecting recovery evidence"`
	Errors              int                   `json:"errors"`
	Items               []MCPSyncRecoveryItem `json:"items"`
	ErrorCode           string                `json:"error_code,omitempty"`
	Error               string                `json:"error,omitempty" jsonschema:"sanitized recovery-diagnostic error"`
}

func mcpSyncRecoveryCallResult(output DiagnoseSyncRecoveryOutput) (*mcp.CallToolResult, DiagnoseSyncRecoveryOutput, error) {
	isError := output.ErrorCode != "" || output.Errors > 0
	return mcpTypedJSONResult("diagnose_sync_recovery", output, output, isError)
}

type mcpSyncRecoveryEvidence struct {
	Index       int
	Decision    string
	Destructive bool
	Post        *syncjournalpkg.Postcondition
	Err         error
}

type mcpSyncRecoveryDiagnosisFingerprint struct {
	Schema           string                                    `json:"schema"`
	ReviewedPlanID   string                                    `json:"reviewed_plan_id"`
	InternalPlanID   string                                    `json:"internal_plan_id"`
	JournalVersion   int                                       `json:"journal_version"`
	JournalUpdatedAt int64                                     `json:"journal_updated_at_unix_nano"`
	Items            []mcpSyncRecoveryDiagnosisFingerprintItem `json:"items"`
}

type mcpSyncRecoveryDiagnosisFingerprintItem struct {
	Index       int                           `json:"index"`
	Action      string                        `json:"action"`
	State       string                        `json:"state"`
	Phase       string                        `json:"phase"`
	Decision    string                        `json:"decision"`
	Destructive bool                          `json:"destructive"`
	Post        *syncjournalpkg.Postcondition `json:"post,omitempty"`
}

func mcpSyncRecoveryDiagnosisID(reviewedPlanID string, journal syncjournalpkg.Journal, evidence []mcpSyncRecoveryEvidence) (string, error) {
	payload := mcpSyncRecoveryDiagnosisFingerprint{
		Schema: "115driver.mcp-sync-recovery-diagnosis/v1", ReviewedPlanID: reviewedPlanID,
		InternalPlanID: journal.PlanID, JournalVersion: journal.Version, JournalUpdatedAt: journal.UpdatedAt.UnixNano(),
		Items: make([]mcpSyncRecoveryDiagnosisFingerprintItem, 0, len(evidence)),
	}
	for _, observed := range evidence {
		if observed.Err != nil || observed.Index < 0 || observed.Index >= len(journal.Items) || observed.Index >= len(journal.Plan.Items) {
			return "", errors.New("recovery diagnosis contains incomplete evidence")
		}
		stored := journal.Items[observed.Index]
		planned := journal.Plan.Items[observed.Index]
		payload.Items = append(payload.Items, mcpSyncRecoveryDiagnosisFingerprintItem{
			Index: observed.Index, Action: planned.Action, State: stored.State, Phase: stored.Phase,
			Decision: observed.Decision, Destructive: observed.Destructive, Post: observed.Post,
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func addMCPRecoveryChecksumEstimate(total *int64, size int64) error {
	if total == nil || size <= 0 {
		return nil
	}
	if size > maxMCPSyncRecoveryChecksumBytes || *total > maxMCPSyncRecoveryChecksumBytes-size {
		return errMCPSyncRecoveryChecksumBudgetExceeded
	}
	*total += size
	return nil
}

func plannedMCPRecoveryChecksumBytes(journal syncjournalpkg.Journal) (int64, error) {
	total := int64(0)
	for _, stored := range journal.Items {
		if stored.Index < 0 || stored.Index >= len(journal.Plan.Items) || stored.State == "succeeded" || stored.State == "skipped" {
			continue
		}
		item := journal.Plan.Items[stored.Index]
		if !syncjournalpkg.IsDestructivePlanItem(item) {
			if stored.Phase == syncjournalpkg.PhaseMutationDone && item.Action == "download" && item.Kind == "file" {
				if err := addMCPRecoveryChecksumEstimate(&total, item.RemoteSize); err != nil {
					return 0, err
				}
			}
			continue
		}
		switch item.Action {
		case "delete-local":
			if item.Kind == "file" {
				if err := addMCPRecoveryChecksumEstimate(&total, item.LocalSize); err != nil {
					return 0, err
				}
			}
		case "replace-local":
			candidate := int64(0)
			if item.ReplacesKind == "file" {
				candidate = item.LocalSize
			}
			if item.Kind == "file" && item.RemoteSize > candidate {
				candidate = item.RemoteSize
			}
			if err := addMCPRecoveryChecksumEstimate(&total, candidate); err != nil {
				return 0, err
			}
		case "replace-remote":
			if item.Kind == "file" {
				if err := addMCPRecoveryChecksumEstimate(&total, item.LocalSize); err != nil {
					return 0, err
				}
			}
		}
	}
	return total, nil
}

func (executor *mcpSyncExecutor) consumeRecoveryChecksum(size int64) error {
	if executor == nil || executor.recoveryChecksumBudget == nil {
		return nil
	}
	return executor.recoveryChecksumBudget.consume(size)
}

func (executor *mcpSyncExecutor) captureRecoveryLocal(localPath string) (*syncjournalpkg.Postcondition, bool, error) {
	validated, err := validateLocalPath(executor.ft.localRoot, localPath, false)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(validated)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("local recovery target is a symlink")
	}
	post := &syncjournalpkg.Postcondition{Side: "local", Exists: true}
	if info.IsDir() {
		post.Kind = "directory"
		return post, true, nil
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("local recovery target is not a regular file")
	}
	if err := executor.consumeRecoveryChecksum(info.Size()); err != nil {
		return nil, false, err
	}
	file, err := os.Open(validated)
	if err != nil {
		return nil, false, err
	}
	digest, digestErr := prepareMCPRecoveryDigest(file, info.Size())
	_ = file.Close()
	if digestErr != nil {
		return nil, false, digestErr
	}
	post.Kind = "file"
	post.Size = info.Size()
	post.SHA1 = digest
	post.ModTimeUnixNano = info.ModTime().UnixNano()
	return post, true, nil
}

func prepareMCPRecoveryDigest(file *os.File, size int64) (string, error) {
	if file == nil {
		return "", errors.New("local recovery file is nil")
	}
	digest, err := uploadpkg.PrepareFileDigest(file, size)
	if err != nil {
		return "", err
	}
	if digest == nil || strings.TrimSpace(digest.SHA1) == "" {
		return "", errors.New("local recovery digest is unavailable")
	}
	return strings.ToUpper(strings.TrimSpace(digest.SHA1)), nil
}

func (executor *mcpSyncExecutor) captureRecoveryRemote(remotePath string) (*syncjournalpkg.Postcondition, bool, error) {
	cleaned := syncplanpkg.CanonicalRemoteRoot(remotePath)
	parent := syncplanpkg.CanonicalRemoteRoot(pathpkg.Dir(cleaned))
	parentID, err := executor.resolver().ResolveDir(parent)
	if err != nil {
		return nil, false, fmt.Errorf("resolve remote recovery parent: %w", err)
	}
	entries, err := executor.ft.client.List(parentID, driver.WithRecordOpenTime(false))
	if err != nil {
		return nil, false, fmt.Errorf("list remote recovery parent: %w", err)
	}
	if entries == nil {
		return nil, false, errors.New("list remote recovery parent returned an empty response")
	}
	name := pathpkg.Base(cleaned)
	match := -1
	for index, entry := range *entries {
		if entry.Name != name {
			continue
		}
		if match >= 0 {
			return nil, false, errors.New("remote recovery target is ambiguous")
		}
		match = index
	}
	if match < 0 {
		return nil, false, nil
	}
	entry := (*entries)[match]
	post := &syncjournalpkg.Postcondition{Side: "remote", Exists: true, RemoteID: strings.TrimSpace(entry.FileID)}
	if entry.IsDirectory {
		post.Kind = "directory"
		return post, true, nil
	}
	post.Kind = "file"
	file, err := executor.ft.client.GetFile(entry.FileID)
	if err != nil {
		return nil, false, fmt.Errorf("read remote recovery target: %w", err)
	}
	if file == nil || file.IsDirectory {
		return nil, false, errors.New("remote recovery target changed type while reading metadata")
	}
	post.Size = file.Size
	post.SHA1 = strings.ToUpper(strings.TrimSpace(file.Sha1))
	if !file.UpdateTime.IsZero() {
		post.ModTimeUnixNano = file.UpdateTime.UnixNano()
	}
	return post, true, nil
}

func (executor *mcpSyncExecutor) recoveryActionTarget(item syncplanpkg.Item) (*syncjournalpkg.Postcondition, bool, error) {
	switch item.Action {
	case "delete-remote", "replace-remote":
		return executor.captureRecoveryRemote(item.RemotePath)
	case "delete-local", "replace-local":
		return executor.captureRecoveryLocal(item.LocalPath)
	default:
		return nil, false, fmt.Errorf("unsupported destructive recovery action %q", item.Action)
	}
}

func (executor *mcpSyncExecutor) recoveryWinnerSourceValid(item syncplanpkg.Item) error {
	switch item.Action {
	case "replace-remote":
		if item.Kind == "directory" {
			return executor.validateLocalDirectorySnapshot(item)
		}
		if err := executor.consumeRecoveryChecksum(item.LocalSize); err != nil {
			return err
		}
		return executor.validateLocalFileSnapshot(item)
	case "replace-local":
		_, err := executor.validateRemoteSnapshot(item, item.Kind)
		return err
	default:
		return fmt.Errorf("unsupported replacement recovery action %q", item.Action)
	}
}

func (executor *mcpSyncExecutor) recoveryWinnerMatches(item syncplanpkg.Item, post *syncjournalpkg.Postcondition) (bool, error) {
	if post == nil || !post.Exists || post.Kind != item.Kind {
		return false, nil
	}
	if item.Kind == "directory" {
		return true, nil
	}
	switch item.Action {
	case "replace-remote":
		if err := executor.consumeRecoveryChecksum(item.LocalSize); err != nil {
			return false, err
		}
		if err := executor.validateLocalFileSnapshot(item); err != nil {
			return false, err
		}
		expected := strings.ToUpper(strings.TrimSpace(item.LocalSHA1))
		return expected != "" && post.Size == item.LocalSize && strings.EqualFold(post.SHA1, expected), nil
	case "replace-local":
		if _, err := executor.validateRemoteSnapshot(item, "file"); err != nil {
			return false, err
		}
		expected := strings.ToUpper(strings.TrimSpace(item.RemoteSHA1))
		return expected != "" && post.Size == item.RemoteSize && strings.EqualFold(post.SHA1, expected), nil
	default:
		return false, fmt.Errorf("unsupported replacement recovery action %q", item.Action)
	}
}

func (executor *mcpSyncExecutor) recoveryOriginalMatches(ctx context.Context, plan syncplanpkg.Plan, item syncplanpkg.Item, post *syncjournalpkg.Postcondition) (bool, error) {
	if post == nil || !post.Exists {
		return false, nil
	}
	expectedKind := item.Kind
	if item.Action == "replace-remote" || item.Action == "replace-local" {
		expectedKind = item.ReplacesKind
	}
	if post.Kind != expectedKind {
		return false, nil
	}
	switch item.Action {
	case "delete-remote", "replace-remote":
		if strings.TrimSpace(item.RemoteID) == "" || post.RemoteID != item.RemoteID {
			return false, nil
		}
		if expectedKind == "file" {
			if post.Size != item.RemoteSize {
				return false, nil
			}
			if item.RemoteSHA1 != "" && (post.SHA1 == "" || !strings.EqualFold(post.SHA1, item.RemoteSHA1)) {
				return false, nil
			}
			return true, nil
		}
		if err := executor.validateRemoteSubtree(ctx, item); err != nil {
			return false, err
		}
		return true, nil
	case "delete-local", "replace-local":
		if expectedKind == "file" {
			if post.Size != item.LocalSize {
				return false, nil
			}
			if item.LocalModTimeUnixNano != 0 && post.ModTimeUnixNano != item.LocalModTimeUnixNano {
				return false, nil
			}
			return true, nil
		}
		if err := executor.validateLocalSubtree(item); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported destructive recovery action %q", item.Action)
	}
}

func (executor *mcpSyncExecutor) diagnoseDestructiveRecoveryEvidence(ctx context.Context, plan syncplanpkg.Plan, item syncplanpkg.Item) (syncjournalpkg.DestructiveDecision, *syncjournalpkg.Postcondition, error) {
	post, exists, err := executor.recoveryActionTarget(item)
	if err != nil {
		return syncjournalpkg.DestructiveAmbiguous, nil, err
	}
	winnerMatches := false
	originalMatches := false
	if item.Action == "replace-remote" || item.Action == "replace-local" {
		if exists {
			winnerMatches, err = executor.recoveryWinnerMatches(item, post)
			if err != nil {
				return syncjournalpkg.DestructiveAmbiguous, post, err
			}
		} else if err := executor.recoveryWinnerSourceValid(item); err != nil {
			return syncjournalpkg.DestructiveAmbiguous, nil, err
		}
	}
	if exists && !winnerMatches {
		originalMatches, err = executor.recoveryOriginalMatches(ctx, plan, item, post)
		if err != nil {
			return syncjournalpkg.DestructiveAmbiguous, post, err
		}
	}
	decision, err := syncjournalpkg.ClassifyDestructiveEvidence(item.Action, exists, winnerMatches, originalMatches)
	if err != nil {
		return syncjournalpkg.DestructiveAmbiguous, post, err
	}
	if decision == syncjournalpkg.DestructiveCompleted && !exists {
		side := "remote"
		if item.Action == "delete-local" {
			side = "local"
		}
		post = &syncjournalpkg.Postcondition{Side: side, Exists: false}
	}
	return decision, post, nil
}

const mcpSyncRecoveryPendingObservation = "pending-observation"

func (executor *mcpSyncExecutor) diagnosePostconditionVerificationEvidence(ctx context.Context, item syncplanpkg.Item) (string, *syncjournalpkg.Postcondition, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	switch item.Action {
	case "upload":
		post, exists, err := executor.captureRecoveryRemote(item.RemotePath)
		if err != nil {
			return "", nil, err
		}
		if !exists {
			return mcpSyncRecoveryPendingObservation, nil, nil
		}
		if post.Kind != item.Kind {
			return string(syncjournalpkg.DestructiveAmbiguous), post, nil
		}
		if item.Kind == "directory" {
			return string(syncjournalpkg.DestructiveCompleted), post, nil
		}
		expectedSHA1 := strings.ToUpper(strings.TrimSpace(item.LocalSHA1))
		if expectedSHA1 == "" {
			return "", post, errors.New("planned upload digest is unavailable for recovery verification")
		}
		if post.Size == item.LocalSize && post.SHA1 != "" && strings.EqualFold(post.SHA1, expectedSHA1) {
			return string(syncjournalpkg.DestructiveCompleted), post, nil
		}
		return string(syncjournalpkg.DestructiveAmbiguous), post, nil
	case "download":
		remote, err := executor.validateRemoteSnapshot(item, item.Kind)
		if err != nil {
			return "", nil, err
		}
		post, exists, err := executor.captureRecoveryLocal(item.LocalPath)
		if err != nil {
			return "", nil, err
		}
		if !exists {
			return mcpSyncRecoveryPendingObservation, nil, nil
		}
		if post.Kind != item.Kind {
			return string(syncjournalpkg.DestructiveAmbiguous), post, nil
		}
		if item.Kind == "directory" {
			return string(syncjournalpkg.DestructiveCompleted), post, nil
		}
		expectedSHA1 := strings.ToUpper(strings.TrimSpace(item.RemoteSHA1))
		if expectedSHA1 == "" && remote != nil {
			expectedSHA1 = strings.ToUpper(strings.TrimSpace(remote.Sha1))
		}
		if expectedSHA1 == "" {
			return "", post, errors.New("planned download digest is unavailable for recovery verification")
		}
		if post.Size == item.RemoteSize && post.SHA1 != "" && strings.EqualFold(post.SHA1, expectedSHA1) {
			return string(syncjournalpkg.DestructiveCompleted), post, nil
		}
		return string(syncjournalpkg.DestructiveAmbiguous), post, nil
	default:
		return "", nil, fmt.Errorf("unsupported postcondition recovery action %q", item.Action)
	}
}

func (executor *mcpSyncExecutor) diagnoseDestructiveRecovery(ctx context.Context, plan syncplanpkg.Plan, item syncplanpkg.Item) (syncjournalpkg.DestructiveDecision, error) {
	decision, _, err := executor.diagnoseDestructiveRecoveryEvidence(ctx, plan, item)
	return decision, err
}

func (ft *FileTools) diagnoseSyncRecoveryJournal(ctx context.Context, reviewedPlanID string, journal syncjournalpkg.Journal) (output DiagnoseSyncRecoveryOutput, evidence []mcpSyncRecoveryEvidence) {
	output = DiagnoseSyncRecoveryOutput{
		Found: true, PlanID: reviewedPlanID, JournalState: journal.State, JournalStatus: syncjournalpkg.EffectiveStatus(journal),
		ReconcileRequired: syncjournalpkg.ReconciliationRequired(journal), EvidenceComplete: true,
		ChecksumBudgetBytes: maxMCPSyncRecoveryChecksumBytes, Items: make([]MCPSyncRecoveryItem, 0),
	}
	plannedBytes, err := plannedMCPRecoveryChecksumBytes(journal)
	if err != nil {
		output.EvidenceComplete = false
		output.ErrorCode = "checksum_budget_exceeded"
		output.Error = "recovery evidence exceeds the fixed local checksum budget"
		return output, nil
	}
	_ = plannedBytes
	budget := newMCPSyncRecoveryChecksumBudget()
	defer func() { output.ChecksummedBytes = budget.used }()
	evidence = make([]mcpSyncRecoveryEvidence, 0)
	executor := &mcpSyncExecutor{ft: ft, plan: journal.Plan, recoveryChecksumBudget: budget}
	for index, stored := range journal.Items {
		if index >= len(journal.Plan.Items) {
			output.EvidenceComplete = false
			output.Errors++
			continue
		}
		item := journal.Plan.Items[index]
		if stored.State == "succeeded" || stored.State == "skipped" || stored.Phase == "" || stored.Phase == syncjournalpkg.PhasePending {
			continue
		}
		destructive := syncjournalpkg.IsDestructivePlanItem(item)
		if !destructive && stored.Phase != syncjournalpkg.PhaseMutationDone {
			continue
		}
		entry := MCPSyncRecoveryItem{
			RelativePath: item.RelativePath, Action: item.Action, Kind: item.Kind,
			State: stored.State, Phase: stored.Phase,
		}
		decision := ""
		var post *syncjournalpkg.Postcondition
		var decisionErr error
		if destructive {
			var destructiveDecision syncjournalpkg.DestructiveDecision
			destructiveDecision, post, decisionErr = executor.diagnoseDestructiveRecoveryEvidence(ctx, journal.Plan, item)
			decision = string(destructiveDecision)
		} else {
			decision, post, decisionErr = executor.diagnosePostconditionVerificationEvidence(ctx, item)
		}
		observed := mcpSyncRecoveryEvidence{Index: index, Decision: decision, Destructive: destructive, Post: post, Err: decisionErr}
		evidence = append(evidence, observed)
		output.Checked++
		if decisionErr != nil {
			entry.Decision = "error"
			entry.HasError = true
			output.Errors++
			output.EvidenceComplete = false
			if errors.Is(decisionErr, errMCPSyncRecoveryChecksumBudgetExceeded) {
				output.ErrorCode = "checksum_budget_exceeded"
				output.Error = "recovery evidence exceeded the fixed local checksum budget"
			}
			output.Items = append(output.Items, entry)
			continue
		}
		entry.Decision = decision
		switch decision {
		case string(syncjournalpkg.DestructiveCompleted):
			output.Completed++
		case string(syncjournalpkg.DestructiveRetryFull):
			output.RetryFull++
		case string(syncjournalpkg.DestructiveWinnerOnly):
			output.WinnerOnly++
		case mcpSyncRecoveryPendingObservation:
			output.PendingObservation++
		default:
			output.Ambiguous++
		}
		output.Items = append(output.Items, entry)
	}
	output.Resolvable = output.EvidenceComplete && output.Errors == 0 && output.Checked > 0 && output.Ambiguous == 0 && output.PendingObservation == 0
	if output.Errors > 0 {
		if output.ErrorCode == "" {
			output.ErrorCode = "evidence_failed"
			output.Error = "one or more recovery items could not be classified safely"
		}
		return output, evidence
	}
	if output.Checked > 0 && output.EvidenceComplete {
		diagnosisID, err := mcpSyncRecoveryDiagnosisID(reviewedPlanID, journal, evidence)
		if err != nil {
			output.EvidenceComplete = false
			output.Resolvable = false
			output.ErrorCode = "diagnosis_failed"
			output.Error = "recovery evidence could not be fingerprinted safely"
			return output, evidence
		}
		output.DiagnosisID = diagnosisID
	}
	return output, evidence
}

func (ft *FileTools) diagnoseSyncRecovery(ctx context.Context, req *mcp.CallToolRequest, args DiagnoseSyncRecoveryArgs) (*mcp.CallToolResult, DiagnoseSyncRecoveryOutput, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(args.PlanID)
	if err != nil || reviewedPlanID == "" {
		return mcpSyncRecoveryCallResult(DiagnoseSyncRecoveryOutput{PlanID: reviewedPlanID, Items: []MCPSyncRecoveryItem{}, ErrorCode: "invalid_plan_id", Error: "plan_id must be a reviewed sha256:<64hex> plan_sync ID"})
	}
	lookup := ft.lookupMCPSyncJournal(ctx, reviewedPlanID)
	if !lookup.found() {
		return mcpSyncRecoveryCallResult(DiagnoseSyncRecoveryOutput{
			Found: lookup.Record != nil, PlanID: reviewedPlanID, Items: []MCPSyncRecoveryItem{},
			ErrorCode: lookup.ErrorCode, Error: lookup.Error,
		})
	}
	record := *lookup.Record
	if record.InUse {
		journal := record.Journal
		return mcpSyncRecoveryCallResult(DiagnoseSyncRecoveryOutput{
			Found: true, PlanID: reviewedPlanID, JournalState: journal.State, JournalStatus: syncjournalpkg.EffectiveStatus(journal),
			InUse: true, ReconcileRequired: syncjournalpkg.ReconciliationRequired(journal), EvidenceComplete: false,
			Items: []MCPSyncRecoveryItem{}, ErrorCode: "execution_in_use", Error: "the sync execution is still in use; recovery evidence is not stable yet",
		})
	}
	output, _ := ft.diagnoseSyncRecoveryJournal(ctx, reviewedPlanID, record.Journal)
	return mcpSyncRecoveryCallResult(output)
}
