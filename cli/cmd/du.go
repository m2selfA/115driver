package cmd

import (
	"fmt"
	"math"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/spf13/cobra"
)

var duMaxDepth int

type duSummary struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Files       int64  `json:"files"`
	Directories int64  `json:"directories"`
	MaxDepth    int    `json:"max_depth"`
	Complete    bool   `json:"complete"`
}

var duCmd = &cobra.Command{
	Use:   "du <remote_path>",
	Short: "Summarize remote file or directory usage",
	Long:  "Recursively sum file bytes and count descendant files/directories. --max-depth limits traversal depth; 0 means unlimited.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if duMaxDepth < 0 {
			return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
		}
		remotePath := args[0]
		fileID, isDir, err := resolver.ResolvePath(client, remotePath)
		if err != nil {
			return &exitError{code: output.ExitNotFound, msg: err.Error()}
		}
		var summary duSummary
		if isDir {
			summary, err = calculateDirectoryUsage(client, fileID, remotePath, duMaxDepth)
		} else {
			file, getErr := client.GetFile(fileID)
			if getErr != nil {
				return &exitError{code: output.ExitError, msg: getErr.Error()}
			}
			summary = duSummary{Path: remotePath, Size: file.Size, Files: 1, MaxDepth: duMaxDepth, Complete: true}
		}
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}
		if jsonOutput {
			printer.PrintSuccess(summary)
			return nil
		}
		status := ""
		if !summary.Complete {
			status = " (depth-limited)"
		}
		fmt.Printf("%s\t%d files\t%d dirs\t%s%s\n", output.FormatFileSize(summary.Size), summary.Files, summary.Directories, summary.Path, status)
		return nil
	},
}

func calculateDirectoryUsage(client remoteTreeListClient, rootID, rootPath string, maxDepth int) (duSummary, error) {
	summary := duSummary{Path: rootPath, MaxDepth: maxDepth, Complete: true}
	walkResult, err := walkRemoteTree(client, rootID, rootPath, maxDepth, func(entry remoteWalkEntry) (bool, error) {
		if entry.File.IsDirectory {
			summary.Directories++
			return false, nil
		}
		if entry.File.Size > 0 && summary.Size > math.MaxInt64-entry.File.Size {
			return false, fmt.Errorf("directory size exceeds int64 while counting %q", entry.RemotePath)
		}
		summary.Files++
		summary.Size += entry.File.Size
		return false, nil
	})
	if err != nil {
		return duSummary{}, err
	}
	summary.Complete = !walkResult.DepthLimited
	return summary, nil
}

func init() {
	duCmd.Flags().IntVar(&duMaxDepth, "max-depth", 0, "Limit recursive traversal depth (0 = unlimited; direct children are depth 1)")
	rootCmd.AddCommand(duCmd)
}
