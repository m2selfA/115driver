package cmd

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	mkdirParents bool
	mkdirDryRun  bool
)

type mkdirResult struct {
	Name  string `json:"name"`
	DirID string `json:"dir_id"`
	Path  string `json:"path"`
}

var mkdirCmd = &cobra.Command{
	Use:   "mkdir [-p] <remote_path>...",
	Short: "Create one or more directories",
	Args:  batchInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		if mkdirDryRun {
			return runMkdirDryRun(cmd, args)
		}
		if len(args) == 1 {
			result, err := createRemoteDirectory(args[0])
			if err != nil {
				return err
			}
			printer.PrintSuccess(result)
			if !jsonOutput {
				fmt.Printf("Created directory: %s (ID: %s)\n", result.Path, result.DirID)
			}
			return nil
		}

		continueOnError := batchContinueOnError(cmd)
		results := make([]mkdirResult, 0, len(args))
		items := make([]batchItemResult, 0, len(args))
		for i, remotePath := range args {
			result, err := createRemoteDirectory(remotePath)
			if err != nil {
				items = append(items, failedBatchItem(remotePath, map[string]interface{}{"path": remotePath}, err))
				printBatchItemFailure(i, len(args), "mkdir "+remotePath, err)
				if !continueOnError {
					break
				}
				continue
			}
			results = append(results, result)
			items = append(items, successfulBatchItem(remotePath, result))
			if !jsonOutput {
				fmt.Printf("Created directory: %s (ID: %s)\n", result.Path, result.DirID)
			}
		}
		data := batchResultData(len(args), items, map[string]interface{}{"created": results})
		if batchFailedCount(items) > 0 {
			return batchIncompleteError("mkdir batch", len(args), items, data)
		}
		printer.PrintSuccess(data)
		return nil
	},
}

func createRemoteDirectory(remotePath string) (mkdirResult, error) {
	remotePath = strings.TrimRight(remotePath, "/")
	if remotePath == "" {
		return mkdirResult{}, &exitError{code: output.ExitArgs, msg: "Cannot create root directory."}
	}
	dirName := path.Base(remotePath)
	parentPath := path.Dir(remotePath)
	if parentPath == "." {
		parentPath = "/"
	}
	if mkdirParents {
		return mkdirP(parentPath, dirName, remotePath)
	}
	parentID, err := resolver.ResolveDir(client, parentPath)
	if err != nil {
		return mkdirResult{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve parent directory %s: %v", parentPath, err)}
	}
	dirID, err := client.Mkdir(parentID, dirName)
	if err != nil {
		return mkdirResult{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	return mkdirResult{Name: dirName, DirID: dirID, Path: remotePath}, nil
}

func mkdirP(parentPath, dirName, fullPath string) (mkdirResult, error) {
	parts := strings.Split(strings.Trim(parentPath+"/"+dirName, "/"), "/")
	currentID := resolver.RootID
	createdPath := ""

	for _, part := range parts {
		if part == "" {
			continue
		}
		createdPath += "/" + part

		existingID, err := resolver.ResolveDir(client, createdPath)
		if err == nil && existingID != "" {
			currentID = existingID
			continue
		}
		if err != nil && !errors.Is(err, driver.ErrNotExist) {
			return mkdirResult{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot inspect directory %s: %v", createdPath, err)}
		}

		newID, err := client.Mkdir(currentID, part)
		if err != nil {
			if errors.Is(err, driver.ErrExist) {
				existingID, resolveErr := resolver.ResolveDir(client, createdPath)
				if resolveErr == nil && existingID != "" {
					currentID = existingID
					continue
				}
				if resolveErr != nil {
					return mkdirResult{}, &exitError{code: classifyRemoteError(resolveErr, output.ExitError), msg: fmt.Sprintf("Directory %s already exists but could not be resolved: %v", createdPath, resolveErr)}
				}
			}
			return mkdirResult{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}
		currentID = newID
	}

	return mkdirResult{Name: dirName, DirID: currentID, Path: fullPath}, nil
}

func init() {
	mkdirCmd.Flags().BoolVarP(&mkdirParents, "parents", "p", false, "Create parent directories as needed")
	mkdirCmd.Flags().BoolVar(&mkdirDryRun, "dry-run", false, "Plan directory creation and parent reuse without creating anything")
	addContinueOnErrorFlag(mkdirCmd)
	addBatchFromFileFlag(mkdirCmd)
	rootCmd.AddCommand(mkdirCmd)
}
