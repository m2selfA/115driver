package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/spf13/cobra"
)

var (
	sessionsGCDryRun      bool
	sessionsGCOlderThan   string
	sessionsRmAbortRemote bool
	sessionsRmDryRun      bool
	sessionsListState     string
	sessionsListDirection string
	sessionsListKind      string
	sessionsListStale     bool
	sessionsListInUse     bool
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Inspect and maintain resumable transfer sessions",
	Args:  cobra.NoArgs,
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List managed transfer sessions",
	Args:  sessionsListArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSessionStore()
		if err != nil {
			return sessionExitError(err)
		}
		entries, err := store.ListSessions()
		if err != nil {
			return sessionExitError(err)
		}
		entries, err = filterSessionEntries(entries, sessionsListState, sessionsListDirection, sessionsListKind, sessionsListStale, sessionsListInUse)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		printer.PrintSuccess(entries)
		if !jsonOutput {
			for _, entry := range entries {
				state := entry.Manifest.State
				if entry.Corrupt {
					state = "corrupt"
				} else if entry.NewerVersion {
					state = fmt.Sprintf("newer-v%d", entry.Manifest.Version)
				}
				flags := ""
				if entry.Stale {
					flags += " stale"
				}
				if entry.InUse {
					flags += " in-use"
				}
				fmt.Printf("%s  %-12s %-8s %s -> %s%s\n", entry.ID, entry.Manifest.Identity.Direction+"/"+entry.Manifest.Identity.Kind, state, entry.Manifest.Identity.LocalPath, entry.Manifest.Identity.RemotePath, flags)
			}
		}
		return nil
	},
}

var sessionsInspectCmd = &cobra.Command{
	Use:   "inspect <id>",
	Short: "Inspect one managed transfer session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSessionStore()
		if err != nil {
			return sessionExitError(err)
		}
		entry, err := store.InspectSession(args[0])
		if err != nil {
			return sessionExitError(err)
		}
		printer.PrintSuccess(entry)
		if !jsonOutput {
			encoded, _ := json.MarshalIndent(entry, "", "  ")
			fmt.Println(string(encoded))
		}
		return nil
	},
}

var sessionsGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect expired managed sessions",
	Args:  sessionsGCArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := auth.ResolveSessionStoreConfig(configPath)
		if err != nil {
			return sessionExitError(err)
		}
		olderThan := time.Duration(0)
		if strings.TrimSpace(sessionsGCOlderThan) != "" {
			olderThan, err = parseSessionAge(sessionsGCOlderThan)
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error()}
			}
		}
		store := transfer.SessionStore{Root: config.SessionDir}
		actions, err := store.RunGCExclusive(transfer.SessionGCOptions{
			Retention: config.SessionRetention, TrashRetention: config.SessionTrashRetention,
			OlderThan: olderThan, DryRun: sessionsGCDryRun,
		})
		if err != nil {
			return sessionExitError(err)
		}
		printer.PrintSuccess(actions)
		if !jsonOutput {
			for _, action := range actions {
				fmt.Printf("%-6s %-16s %s\n", action.Action, action.Reason, action.Path)
			}
			if len(actions) == 0 {
				fmt.Println("No session maintenance needed.")
			}
		}
		return nil
	},
}

type sessionRemoveResult struct {
	ID                     string `json:"id"`
	TrashPath              string `json:"trash_path"`
	RemoteMultipartAborted int    `json:"remote_multipart_aborted"`
}

type sessionRemovePlan struct {
	Input string
	ID    string
	Entry transfer.SessionEntry
	Err   error
}

var sessionsRmCmd = &cobra.Command{
	Use:   "rm <id>...",
	Short: "Move one or more managed sessions to trash",
	Args:  batchInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		sessionConfig, err := auth.ResolveSessionStoreConfig(configPath)
		if err != nil {
			return sessionExitError(err)
		}
		store := transfer.SessionStore{Root: sessionConfig.SessionDir}
		uploadOptions := uploadpkg.Options{}
		if sessionsRmAbortRemote {
			transferConfig, configErr := auth.ResolveTransferConfig(configPath)
			if configErr != nil {
				return sessionExitError(configErr)
			}
			uploadOptions, err = buildUploadOptions(transferConfig, "", "", uploadpkg.DefaultTimeout)
			if err != nil {
				return sessionExitError(err)
			}
		}
		if sessionsRmDryRun {
			return runSessionsRemoveDryRun(cmd, store, args)
		}
		if len(args) == 1 {
			result, err := removeManagedSession(cmd.Context(), store, args[0], sessionsRmAbortRemote, uploadOptions)
			if err != nil {
				return err
			}
			printer.PrintSuccess(map[string]interface{}{"trash_path": result.TrashPath, "remote_multipart_aborted": result.RemoteMultipartAborted})
			printSessionRemoveResult(result)
			return nil
		}

		continueOnError := batchContinueOnError(cmd)
		plans := prepareSessionRemovePlans(store, args)
		if !continueOnError {
			for _, plan := range plans {
				if plan.Err != nil {
					return plan.Err
				}
			}
		}
		removed := make([]sessionRemoveResult, 0, len(args))
		items := make([]batchItemResult, 0, len(args))
		for i, plan := range plans {
			if plan.Err != nil {
				items = append(items, failedBatchItem(plan.Input, map[string]interface{}{"id": plan.Input}, plan.Err))
				printBatchItemFailure(i, len(args), "sessions rm "+plan.Input, plan.Err)
				continue
			}
			result, err := removeManagedSession(cmd.Context(), store, plan.ID, sessionsRmAbortRemote, uploadOptions)
			if err != nil {
				items = append(items, failedBatchItem(plan.Input, map[string]interface{}{"id": plan.ID}, err))
				printBatchItemFailure(i, len(args), "sessions rm "+plan.Input, err)
				if !continueOnError {
					break
				}
				continue
			}
			removed = append(removed, result)
			items = append(items, successfulBatchItem(plan.Input, result))
			printSessionRemoveResult(result)
		}
		data := batchResultData(len(args), items, map[string]interface{}{"trashed": removed})
		if batchFailedCount(items) > 0 {
			return batchIncompleteError("sessions rm batch", len(args), items, data)
		}
		printer.PrintSuccess(data)
		return nil
	},
}

func prepareSessionRemovePlans(store transfer.SessionStore, inputs []string) []sessionRemovePlan {
	plans := make([]sessionRemovePlan, len(inputs))
	seen := make(map[string]string, len(inputs))
	for i, input := range inputs {
		plans[i].Input = input
		entry, err := store.InspectSession(input)
		if err != nil {
			plans[i].Err = sessionExitError(err)
			continue
		}
		if previous, ok := seen[entry.ID]; ok {
			plans[i].Err = &exitError{code: output.ExitArgs, msg: fmt.Sprintf("session %q resolves to the same session as %q (%s)", input, previous, entry.ID)}
			continue
		}
		seen[entry.ID] = input
		plans[i].ID = entry.ID
		plans[i].Entry = entry
	}
	return plans
}

func removeManagedSession(ctx context.Context, store transfer.SessionStore, id string, abortRemote bool, options uploadpkg.Options) (sessionRemoveResult, error) {
	result := sessionRemoveResult{ID: id}
	var beforeTrash func(transfer.SessionEntry) error
	if abortRemote {
		beforeTrash = func(entry transfer.SessionEntry) error {
			count, err := abortRemoteMultipartSession(ctx, entry, options)
			result.RemoteMultipartAborted = count
			return err
		}
	}
	trashPath, err := store.TrashSessionWithHook(id, "manual", beforeTrash)
	if err != nil {
		return result, sessionExitError(err)
	}
	result.TrashPath = trashPath
	return result, nil
}

func printSessionRemoveResult(result sessionRemoveResult) {
	if jsonOutput {
		return
	}
	if sessionsRmAbortRemote {
		fmt.Printf("Remote multipart uploads aborted: %d\n", result.RemoteMultipartAborted)
	}
	fmt.Printf("Session moved to trash: %s\n", result.TrashPath)
}

func abortRemoteMultipartSession(ctx context.Context, entry transfer.SessionEntry, options uploadpkg.Options) (int, error) {
	if entry.Corrupt || entry.NewerVersion {
		return 0, fmt.Errorf("cannot abort remote multipart for an unreadable session")
	}
	if entry.Manifest.Identity.Direction != "upload" {
		return 0, fmt.Errorf("--abort-remote is only valid for upload sessions")
	}
	if client == nil || client.UserID == 0 {
		return 0, fmt.Errorf("--abort-remote requires an authenticated 115 account")
	}
	if entry.Manifest.AccountID == 0 {
		return 0, fmt.Errorf("session account is not bound; resume it once before using --abort-remote")
	}
	if entry.Manifest.AccountID != client.UserID {
		return 0, fmt.Errorf("session belongs to account %d, current account is %d", entry.Manifest.AccountID, client.UserID)
	}
	paths, err := uploadSessionResumePaths(entry)
	if err != nil {
		return 0, err
	}
	aborted := 0
	for _, path := range paths {
		active, err := uploadpkg.AbortRemoteResumeMultipart(ctx, client, path, options)
		if err != nil {
			return aborted, err
		}
		if active {
			aborted++
		}
	}
	return aborted, nil
}

func sessionsListArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if err := validateSessionFilters(sessionsListState, sessionsListDirection, sessionsListKind); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return nil
}

func sessionsGCArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if strings.TrimSpace(sessionsGCOlderThan) != "" {
		if _, err := parseSessionAge(sessionsGCOlderThan); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	return nil
}

func validateSessionFilters(state, direction, kind string) error {
	state = strings.ToLower(strings.TrimSpace(state))
	direction = strings.ToLower(strings.TrimSpace(direction))
	kind = strings.ToLower(strings.TrimSpace(kind))
	if state != "" && state != "active" && state != "completed" && state != "corrupt" && state != "newer" {
		return fmt.Errorf("invalid session state %q; expected active, completed, corrupt, or newer", state)
	}
	if direction != "" && direction != "upload" && direction != "download" {
		return fmt.Errorf("invalid session direction %q; expected upload or download", direction)
	}
	if kind != "" && kind != "file" && kind != "tree" {
		return fmt.Errorf("invalid session kind %q; expected file or tree", kind)
	}
	return nil
}

func filterSessionEntries(entries []transfer.SessionEntry, state, direction, kind string, staleOnly, inUseOnly bool) ([]transfer.SessionEntry, error) {
	if err := validateSessionFilters(state, direction, kind); err != nil {
		return nil, err
	}
	state = strings.ToLower(strings.TrimSpace(state))
	direction = strings.ToLower(strings.TrimSpace(direction))
	kind = strings.ToLower(strings.TrimSpace(kind))
	filtered := make([]transfer.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		entryState := entry.Manifest.State
		if entry.Corrupt {
			entryState = "corrupt"
		} else if entry.NewerVersion {
			entryState = "newer"
		}
		if state != "" && entryState != state {
			continue
		}
		if direction != "" && strings.ToLower(entry.Manifest.Identity.Direction) != direction {
			continue
		}
		if kind != "" && strings.ToLower(entry.Manifest.Identity.Kind) != kind {
			continue
		}
		if staleOnly && !entry.Stale {
			continue
		}
		if inUseOnly && !entry.InUse {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

func resolveSessionStore() (transfer.SessionStore, error) {
	config, err := auth.ResolveSessionStoreConfig(configPath)
	if err != nil {
		return transfer.SessionStore{}, err
	}
	return transfer.SessionStore{Root: config.SessionDir}, nil
}

func sessionExitError(err error) error {
	code := output.ExitError
	if errors.Is(err, os.ErrNotExist) {
		code = output.ExitNotFound
	}
	if errors.Is(err, transfer.ErrSessionLocked) || errors.Is(err, transfer.ErrSessionNewerVersion) {
		code = output.ExitArgs
	}
	return &exitError{code: code, msg: err.Error()}
}

func parseSessionAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid session age %q", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid session age %q", value)
	}
	return duration, nil
}

func init() {
	sessionsListCmd.Flags().StringVar(&sessionsListState, "state", "", "Filter by state: active, completed, corrupt, or newer")
	sessionsListCmd.Flags().StringVar(&sessionsListDirection, "direction", "", "Filter by direction: upload or download")
	sessionsListCmd.Flags().StringVar(&sessionsListKind, "kind", "", "Filter by kind: file or tree")
	sessionsListCmd.Flags().BoolVar(&sessionsListStale, "stale", false, "Show only stale active sessions")
	sessionsListCmd.Flags().BoolVar(&sessionsListInUse, "in-use", false, "Show only currently locked sessions")
	sessionsGCCmd.Flags().BoolVar(&sessionsGCDryRun, "dry-run", false, "Show maintenance actions without changing sessions")
	sessionsGCCmd.Flags().StringVar(&sessionsGCOlderThan, "older-than", "", "Override active-session retention (for example 60d or 720h)")
	sessionsRmCmd.Flags().BoolVar(&sessionsRmAbortRemote, "abort-remote", false, "Abort active OSS multipart uploads before moving the session to trash (requires authentication)")
	sessionsRmCmd.Flags().BoolVar(&sessionsRmDryRun, "dry-run", false, "Plan session removal and remote multipart aborts without changing session or remote state")
	addContinueOnErrorFlag(sessionsRmCmd)
	addBatchFromFileFlag(sessionsRmCmd)
	sessionsCmd.AddCommand(sessionsListCmd, sessionsInspectCmd, sessionsGCCmd, sessionsRmCmd)
	rootCmd.AddCommand(sessionsCmd)
}
