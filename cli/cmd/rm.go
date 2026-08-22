package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

var (
	rmForce  bool
	rmDryRun bool
)

var rmCmd = &cobra.Command{
	Use:   "rm <remote_path>...",
	Short: "Delete files or directories (moves to recycle bin)",
	Long:  "Delete one or more remote files/directories. Deleting a directory already deletes its complete subtree, so no --recursive flag is required. --dry-run resolves IDs and recursively summarizes directory subtrees without prompting or moving anything to the recycle bin.",
	Args:  rmInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fromFile, err := batchFromFile(cmd)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if fromFile == "-" && !rmForce && !rmDryRun {
			return &exitError{code: output.ExitArgs, msg: "rm --from-file - requires --force because stdin is reserved for the source list"}
		}
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		items, err := resolveUniqueRemoteItems(client, args)
		if err != nil {
			return err
		}
		if err := validateRemoteMutationItems(items); err != nil {
			return err
		}
		if rmDryRun {
			plan, err := buildRemoteDeletePlanFromItems(client, items)
			if err != nil {
				return err
			}
			printer.PrintSuccess(plan)
			printRemoteDeletePlan(plan)
			return nil
		}
		hasDirectory := false
		fileIDs := make([]string, 0, len(items))
		remotePaths := make([]string, 0, len(items))
		for _, item := range items {
			hasDirectory = hasDirectory || item.IsDir
			fileIDs = append(fileIDs, item.ID)
			remotePaths = append(remotePaths, item.Path)
		}
		if err := validateDeleteConfirmation(hasDirectory, jsonOutput, rmForce); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}

		if hasDirectory && !jsonOutput && !rmForce {
			if len(items) == 1 && items[0].IsDir {
				fmt.Printf("Delete directory %s and all its contents? [y/N] ", items[0].Path)
			} else {
				fmt.Printf("Delete %d items, including directories and all their contents? [y/N] ", len(items))
			}
			reader := bufio.NewReader(os.Stdin)
			resp, _ := reader.ReadString('\n')
			resp = strings.TrimSpace(strings.ToLower(resp))
			if resp != "y" && resp != "yes" {
				fmt.Println("Canceled.")
				return nil
			}
		}

		if err := client.Delete(fileIDs...); err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}

		printer.PrintSuccess(map[string]interface{}{
			"deleted": remotePaths, "file_ids": fileIDs,
		})
		if !jsonOutput {
			if len(remotePaths) == 1 {
				fmt.Printf("Deleted: %s\n", remotePaths[0])
			} else {
				fmt.Printf("Deleted %d items.\n", len(remotePaths))
			}
		}
		return nil
	},
}

func rmInputArgs(cmd *cobra.Command, args []string) error {
	if err := batchInputArgs(cmd, args); err != nil {
		return err
	}
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if fromFile == "-" && !rmForce && !rmDryRun {
		return &exitError{code: output.ExitArgs, msg: "rm --from-file - requires --force because stdin is reserved for the source list"}
	}
	return nil
}

func init() {
	rmCmd.Flags().BoolVarP(&rmForce, "force", "f", false, "Skip confirmation for directory deletes")
	rmCmd.Flags().BoolVar(&rmDryRun, "dry-run", false, "Plan deletion and summarize directory subtrees without deleting or prompting")
	addBatchFromFileFlag(rmCmd)
	rootCmd.AddCommand(rmCmd)
}

func validateDeleteConfirmation(isDir, jsonOutput, force bool) error {
	if !isDir {
		return nil
	}
	if force {
		return nil
	}
	if jsonOutput {
		return errors.New("directory delete requires --force when using --json")
	}
	return nil
}
