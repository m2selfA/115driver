package syncexec

import (
	"context"
	"errors"
	"fmt"
	"sort"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

var (
	ErrPlanNotReady         = errors.New("sync plan has unresolved conflicts")
	ErrDestructiveApproval  = errors.New("sync plan contains destructive actions that require --allow-destructive")
	ErrExecutionPreparation = errors.New("sync execution preparation failed")
)

type Deps struct {
	ForcePreflight        bool
	BeforeItem            func(context.Context, int, syncplanpkg.Item) error
	AfterItem             func(context.Context, int, syncplanpkg.Item, Outcome) error
	Preflight             func(context.Context) error
	Prepare               func() error
	Parallelism           func() (fileTransferSlots, workersPerTransfer int)
	AcquireFileTransfer   func(context.Context) (func(), error)
	CreateRemoteDirectory func(context.Context, syncplanpkg.Item) error
	RemoveRemote          func(context.Context, syncplanpkg.Item) error
	DeleteRemote          func(context.Context, syncplanpkg.Item) error
	UploadFile            func(context.Context, syncplanpkg.Item) error
	CreateLocalDirectory  func(context.Context, syncplanpkg.Item) error
	RemoveLocal           func(context.Context, syncplanpkg.Item) error
	DeleteLocal           func(context.Context, syncplanpkg.Item) error
	DownloadFile          func(context.Context, syncplanpkg.Item) error
}

type ItemResult struct {
	RelativePath string `json:"relative_path"`
	Action       string `json:"action"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

type Summary struct {
	Operation              string       `json:"operation"`
	PlanID                 string       `json:"plan_id"`
	DryRun                 bool         `json:"dry_run"`
	Direction              string       `json:"direction"`
	ConflictPolicy         string       `json:"conflict_policy"`
	DeleteExtraneous       bool         `json:"delete"`
	AllowDestructive       bool         `json:"allow_destructive"`
	ContinueOnError        bool         `json:"continue_on_error"`
	MaxErrors              int          `json:"max_errors"`
	JournalEnabled         bool         `json:"journal_enabled"`
	JournalResumed         bool         `json:"journal_resumed"`
	JournalCompletedBefore int          `json:"journal_completed_before"`
	JournalVersion         int          `json:"journal_version,omitempty"`
	JournalState           string       `json:"journal_state,omitempty"`
	JournalStatus          string       `json:"journal_status,omitempty"`
	Jobs                   int          `json:"jobs"`
	FileTransferSlots      int          `json:"file_transfer_slots"`
	WorkersPerTransfer     int          `json:"workers_per_transfer"`
	PlannedItems           int          `json:"planned_items"`
	PreflightChecked       int          `json:"preflight_checked"`
	PreflightPassed        bool         `json:"preflight_passed"`
	Processed              int          `json:"processed"`
	Succeeded              int          `json:"succeeded"`
	Skipped                int          `json:"skipped"`
	Failed                 int          `json:"failed"`
	Blocked                int          `json:"blocked"`
	UploadedFiles          int          `json:"uploaded_files"`
	CreatedRemoteDirs      int          `json:"created_remote_directories"`
	DownloadedFiles        int          `json:"downloaded_files"`
	CreatedLocalDirs       int          `json:"created_local_directories"`
	ReplacedRemote         int          `json:"replaced_remote"`
	ReplacedLocal          int          `json:"replaced_local"`
	DeletedRemote          int          `json:"deleted_remote"`
	DeletedLocal           int          `json:"deleted_local"`
	DestructiveActions     int          `json:"destructive_actions"`
	Items                  []ItemResult `json:"items"`
}

func NewSummary(plan syncplanpkg.Plan, allowDestructive bool, jobs int) Summary {
	return Summary{
		Operation: "sync", PlanID: plan.PlanID, Direction: plan.Direction, ConflictPolicy: plan.ConflictPolicy, DeleteExtraneous: plan.DeleteExtraneous, AllowDestructive: allowDestructive, Jobs: jobs,
		PlannedItems: len(plan.Items), DestructiveActions: plan.DestructiveActions,
		Items: make([]ItemResult, 0, len(plan.Items)),
	}
}

type Outcome struct {
	Index             int
	Result            ItemResult
	UploadedFiles     int
	CreatedRemoteDirs int
	DownloadedFiles   int
	CreatedLocalDirs  int
	ReplacedRemote    int
	ReplacedLocal     int
	DeletedRemote     int
	DeletedLocal      int
	Err               error
}

func ValidateFailurePolicy(continueOnError bool, maxErrors int) error {
	if maxErrors < 0 {
		return fmt.Errorf("--max-errors must be >= 0")
	}
	if maxErrors > 0 && !continueOnError {
		return fmt.Errorf("--max-errors requires --continue-on-error")
	}
	return nil
}

func ValidateSafety(plan syncplanpkg.Plan, allowDestructive bool) error {
	if !plan.Ready || plan.Conflicts > 0 {
		return ErrPlanNotReady
	}
	if plan.DestructiveActions > 0 && !allowDestructive {
		return ErrDestructiveApproval
	}
	return nil
}

func ValidateDeps(plan syncplanpkg.Plan, deps Deps) error {
	if (PlanHasWrites(plan) || deps.ForcePreflight) && deps.Preflight == nil {
		return errors.New("sync execution preflight is nil")
	}
	for _, item := range plan.Items {
		switch item.Action {
		case "skip":
		case "upload":
			if item.Kind == "directory" && deps.CreateRemoteDirectory == nil {
				return errors.New("sync remote-directory executor is nil")
			}
			if item.Kind != "directory" && deps.UploadFile == nil {
				return errors.New("sync upload executor is nil")
			}
		case "download":
			if item.Kind == "directory" && deps.CreateLocalDirectory == nil {
				return errors.New("sync local-directory executor is nil")
			}
			if item.Kind != "directory" && deps.DownloadFile == nil {
				return errors.New("sync download executor is nil")
			}
		case "delete-remote":
			if deps.DeleteRemote == nil {
				return errors.New("sync remote delete executor is nil")
			}
		case "delete-local":
			if deps.DeleteLocal == nil {
				return errors.New("sync local delete executor is nil")
			}
		case "replace-remote":
			if deps.RemoveRemote == nil {
				return errors.New("sync remote replacement executor is nil")
			}
			if item.Kind == "directory" && deps.CreateRemoteDirectory == nil {
				return errors.New("sync remote-directory executor is nil")
			}
			if item.Kind != "directory" && deps.UploadFile == nil {
				return errors.New("sync upload executor is nil")
			}
		case "replace-local":
			if deps.RemoveLocal == nil {
				return errors.New("sync local replacement executor is nil")
			}
			if item.Kind == "directory" && deps.CreateLocalDirectory == nil {
				return errors.New("sync local-directory executor is nil")
			}
			if item.Kind != "directory" && deps.DownloadFile == nil {
				return errors.New("sync download executor is nil")
			}
		case "conflict":
			return ErrPlanNotReady
		default:
			return fmt.Errorf("unsupported sync plan action %q", item.Action)
		}
	}
	return nil
}

func Execute(ctx context.Context, plan syncplanpkg.Plan, allowDestructive bool, deps Deps) (Summary, error) {
	return ExecuteWithJobsFailurePolicy(ctx, plan, allowDestructive, 1, false, 0, deps)
}

func ExecuteWithJobs(ctx context.Context, plan syncplanpkg.Plan, allowDestructive bool, jobs int, deps Deps) (Summary, error) {
	return ExecuteWithJobsFailurePolicy(ctx, plan, allowDestructive, jobs, false, 0, deps)
}

func ExecuteWithJobsPolicy(ctx context.Context, plan syncplanpkg.Plan, allowDestructive bool, jobs int, continueOnError bool, deps Deps) (Summary, error) {
	return ExecuteWithJobsFailurePolicy(ctx, plan, allowDestructive, jobs, continueOnError, 0, deps)
}

func ExecuteWithJobsFailurePolicy(ctx context.Context, plan syncplanpkg.Plan, allowDestructive bool, requestedJobs int, continueOnError bool, maxErrors int, deps Deps) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jobs, err := ResolveJobs(requestedJobs)
	summary := NewSummary(plan, allowDestructive, requestedJobs)
	summary.ContinueOnError = continueOnError
	summary.MaxErrors = maxErrors
	if err != nil {
		return summary, err
	}
	summary.Jobs = jobs
	if err := ValidateFailurePolicy(continueOnError, maxErrors); err != nil {
		return summary, err
	}
	if err := ValidateSafety(plan, allowDestructive); err != nil {
		return summary, err
	}
	if err := ValidateDeps(plan, deps); err != nil {
		return summary, err
	}
	graph, err := BuildGraph(plan)
	if err != nil {
		return summary, err
	}
	if PlanHasWrites(plan) || deps.ForcePreflight {
		if err := deps.Preflight(ctx); err != nil {
			return summary, fmt.Errorf("sync preflight failed: %w", err)
		}
		summary.PreflightChecked = len(plan.Items)
	}
	summary.PreflightPassed = true
	if deps.Prepare != nil {
		if err := deps.Prepare(); err != nil {
			return summary, fmt.Errorf("%w: %w", ErrExecutionPreparation, err)
		}
	}
	if deps.Parallelism != nil {
		summary.FileTransferSlots, summary.WorkersPerTransfer = deps.Parallelism()
	}
	return executeGraph(ctx, plan, jobs, continueOnError, maxErrors, deps, graph, summary)
}

func executeGraph(ctx context.Context, plan syncplanpkg.Plan, jobs int, continueOnError bool, maxErrors int, deps Deps, graph Graph, summary Summary) (Summary, error) {
	indegree := make([]int, len(graph.Dependencies))
	ready := make([]int, 0, len(indegree))
	for index, dependencies := range graph.Dependencies {
		indegree[index] = len(dependencies)
		if indegree[index] == 0 {
			ready = append(ready, index)
		}
	}
	sort.Ints(ready)
	outcomes := make([]*Outcome, len(plan.Items))
	blocked := make([]bool, len(plan.Items))
	terminal := 0
	var firstFailure error
	failureCount := 0
	for terminal < len(plan.Items) {
		if err := ctx.Err(); err != nil {
			return FinalizeSummary(summary, outcomes), err
		}
		if len(ready) == 0 {
			return FinalizeSummary(summary, outcomes), fmt.Errorf("sync execution dependency graph stalled after %d of %d terminal item(s)", terminal, len(plan.Items))
		}
		waveSize := jobs
		if waveSize > len(ready) {
			waveSize = len(ready)
		}
		wave := append([]int(nil), ready[:waveSize]...)
		ready = ready[waveSize:]
		done := make(chan Outcome, waveSize)
		for _, index := range wave {
			item := plan.Items[index]
			go func(index int, item syncplanpkg.Item) {
				done <- RunItem(ctx, index, item, deps)
			}(index, item)
		}
		waveOutcomes := make([]Outcome, 0, waveSize)
		for range wave {
			waveOutcomes = append(waveOutcomes, <-done)
		}
		sort.Slice(waveOutcomes, func(i, j int) bool { return waveOutcomes[i].Index < waveOutcomes[j].Index })
		var waveErr error
		for _, outcome := range waveOutcomes {
			copy := outcome
			outcomes[outcome.Index] = &copy
			terminal++
			if outcome.Err != nil {
				failureCount++
				item := plan.Items[outcome.Index]
				failure := fmt.Errorf("sync %s %q failed: %w", item.Action, item.RelativePath, outcome.Err)
				if firstFailure == nil {
					firstFailure = failure
				}
				if waveErr == nil {
					waveErr = failure
				}
				if continueOnError {
					terminal += BlockDependents(outcome.Index, item.RelativePath, plan, graph, blocked, outcomes)
				}
				continue
			}
			for _, dependent := range graph.Dependents[outcome.Index] {
				if blocked[dependent] {
					continue
				}
				indegree[dependent]--
				if indegree[dependent] == 0 {
					ready = append(ready, dependent)
				}
			}
		}
		sort.Ints(ready)
		if waveErr != nil && !continueOnError {
			return FinalizeSummary(summary, outcomes), waveErr
		}
		if continueOnError && maxErrors > 0 && failureCount >= maxErrors {
			final := FinalizeSummary(summary, outcomes)
			return final, fmt.Errorf("sync execution stopped after reaching --max-errors %d with %d failed action(s) and %d blocked dependent item(s); first failure: %w", maxErrors, final.Failed, final.Blocked, firstFailure)
		}
	}
	final := FinalizeSummary(summary, outcomes)
	if firstFailure != nil {
		return final, fmt.Errorf("sync execution completed with %d failed action(s) and %d blocked dependent item(s); first failure: %w", final.Failed, final.Blocked, firstFailure)
	}
	return final, nil
}

func BlockDependents(failedIndex int, failedPath string, plan syncplanpkg.Plan, graph Graph, blocked []bool, outcomes []*Outcome) int {
	queue := append([]int(nil), graph.Dependents[failedIndex]...)
	marked := 0
	for len(queue) > 0 {
		index := queue[0]
		queue = queue[1:]
		if index < 0 || index >= len(plan.Items) || blocked[index] || outcomes[index] != nil {
			continue
		}
		blocked[index] = true
		item := plan.Items[index]
		outcomes[index] = &Outcome{
			Index: index,
			Result: ItemResult{
				RelativePath: item.RelativePath,
				Action:       item.Action,
				Status:       "blocked",
				Error:        "blocked by failed dependency: " + failedPath,
			},
		}
		marked++
		queue = append(queue, graph.Dependents[index]...)
	}
	return marked
}

func RunItem(ctx context.Context, index int, item syncplanpkg.Item, deps Deps) Outcome {
	outcome := Outcome{Index: index, Result: ItemResult{RelativePath: item.RelativePath, Action: item.Action}}
	if err := ctx.Err(); err != nil {
		outcome.Err = err
	} else if deps.BeforeItem != nil {
		outcome.Err = deps.BeforeItem(ctx, index, item)
	}
	if outcome.Err == nil && ItemUsesFileTransfer(item) && deps.AcquireFileTransfer != nil {
		release, err := deps.AcquireFileTransfer(ctx)
		if err != nil {
			outcome.Err = err
		} else {
			defer release()
		}
	}
	if outcome.Err == nil {
		switch item.Action {
		case "skip":
			outcome.Result.Status = "skipped"
		case "upload":
			if item.Kind == "directory" {
				outcome.Err = deps.CreateRemoteDirectory(ctx, item)
				if outcome.Err == nil {
					outcome.CreatedRemoteDirs = 1
				}
			} else {
				outcome.Err = deps.UploadFile(ctx, item)
				if outcome.Err == nil {
					outcome.UploadedFiles = 1
				}
			}
		case "download":
			if item.Kind == "directory" {
				outcome.Err = deps.CreateLocalDirectory(ctx, item)
				if outcome.Err == nil {
					outcome.CreatedLocalDirs = 1
				}
			} else {
				outcome.Err = deps.DownloadFile(ctx, item)
				if outcome.Err == nil {
					outcome.DownloadedFiles = 1
				}
			}
		case "delete-remote":
			outcome.Err = deps.DeleteRemote(ctx, item)
			if outcome.Err == nil {
				outcome.DeletedRemote = 1
			}
		case "delete-local":
			outcome.Err = deps.DeleteLocal(ctx, item)
			if outcome.Err == nil {
				outcome.DeletedLocal = 1
			}
		case "replace-remote":
			outcome.Err = deps.RemoveRemote(ctx, item)
			if outcome.Err == nil {
				if item.Kind == "directory" {
					outcome.Err = deps.CreateRemoteDirectory(ctx, item)
					if outcome.Err == nil {
						outcome.CreatedRemoteDirs = 1
					}
				} else {
					outcome.Err = deps.UploadFile(ctx, item)
					if outcome.Err == nil {
						outcome.UploadedFiles = 1
					}
				}
			}
			if outcome.Err == nil {
				outcome.ReplacedRemote = 1
			}
		case "replace-local":
			outcome.Err = deps.RemoveLocal(ctx, item)
			if outcome.Err == nil {
				if item.Kind == "directory" {
					outcome.Err = deps.CreateLocalDirectory(ctx, item)
					if outcome.Err == nil {
						outcome.CreatedLocalDirs = 1
					}
				} else {
					outcome.Err = deps.DownloadFile(ctx, item)
					if outcome.Err == nil {
						outcome.DownloadedFiles = 1
					}
				}
			}
			if outcome.Err == nil {
				outcome.ReplacedLocal = 1
			}
		default:
			outcome.Err = fmt.Errorf("unsupported sync plan action %q", item.Action)
		}
	}
	if outcome.Err != nil {
		outcome.Result.Status = "failed"
		outcome.Result.Error = outcome.Err.Error()
	} else if outcome.Result.Status == "" {
		outcome.Result.Status = "succeeded"
	}
	if deps.AfterItem != nil {
		if err := deps.AfterItem(ctx, index, item, outcome); err != nil {
			outcome.Err = errors.Join(outcome.Err, err)
			outcome.Result.Status = "failed"
			outcome.Result.Error = outcome.Err.Error()
		}
	}
	return outcome
}

func FinalizeSummary(summary Summary, outcomes []*Outcome) Summary {
	summary.Processed = 0
	summary.Succeeded = 0
	summary.Skipped = 0
	summary.Failed = 0
	summary.Blocked = 0
	summary.UploadedFiles = 0
	summary.CreatedRemoteDirs = 0
	summary.DownloadedFiles = 0
	summary.CreatedLocalDirs = 0
	summary.ReplacedRemote = 0
	summary.ReplacedLocal = 0
	summary.DeletedRemote = 0
	summary.DeletedLocal = 0
	summary.Items = summary.Items[:0]
	for _, outcome := range outcomes {
		if outcome == nil {
			continue
		}
		if outcome.Result.Status != "blocked" {
			summary.Processed++
		}
		switch outcome.Result.Status {
		case "succeeded":
			summary.Succeeded++
		case "skipped":
			summary.Skipped++
		case "failed":
			summary.Failed++
		case "blocked":
			summary.Blocked++
		}
		summary.UploadedFiles += outcome.UploadedFiles
		summary.CreatedRemoteDirs += outcome.CreatedRemoteDirs
		summary.DownloadedFiles += outcome.DownloadedFiles
		summary.CreatedLocalDirs += outcome.CreatedLocalDirs
		summary.ReplacedRemote += outcome.ReplacedRemote
		summary.ReplacedLocal += outcome.ReplacedLocal
		summary.DeletedRemote += outcome.DeletedRemote
		summary.DeletedLocal += outcome.DeletedLocal
		summary.Items = append(summary.Items, outcome.Result)
	}
	return summary
}
