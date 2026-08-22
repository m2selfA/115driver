package cmd

import (
	"fmt"
	"math"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
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
	Use:   "du <remote_path>...",
	Short: "Summarize usage for one or more remote paths",
	Long:  "Recursively sum file bytes and count descendant files/directories for each requested path. --max-depth limits traversal depth; 0 means unlimited.",
	Args:  duInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if duMaxDepth < 0 {
			return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
		}
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		if len(args) == 1 {
			summary, err := summarizeRemoteUsage(client, args[0], duMaxDepth)
			if err != nil {
				return err
			}
			if jsonOutput {
				printer.PrintSuccess(summary)
			} else {
				printDUSummary(summary)
			}
			return nil
		}

		continueOnError := batchContinueOnError(cmd)
		summaries := make([]duSummary, 0, len(args))
		items := make([]batchItemResult, 0, len(args))
		pathResolver := resolver.New(client)
		for i, remotePath := range args {
			summary, err := summarizeRemoteUsageWithResolver(client, pathResolver, remotePath, duMaxDepth)
			if err != nil {
				items = append(items, failedBatchItem(remotePath, map[string]interface{}{"path": remotePath}, err))
				printBatchItemFailure(i, len(args), "du "+remotePath, err)
				if !continueOnError {
					break
				}
				continue
			}
			summaries = append(summaries, summary)
			items = append(items, successfulBatchItem(remotePath, summary))
		}
		if !jsonOutput {
			for _, summary := range summaries {
				printDUSummary(summary)
			}
		}
		data := batchResultData(len(args), items, map[string]interface{}{"entries": summaries})
		if batchFailedCount(items) > 0 {
			return batchIncompleteError("du batch", len(args), items, data)
		}
		printer.PrintSuccess(data)
		return nil
	},
}

func duInputArgs(cmd *cobra.Command, args []string) error {
	if duMaxDepth < 0 {
		return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
	}
	return batchInputArgs(cmd, args)
}

func printDUSummary(summary duSummary) {
	status := ""
	if !summary.Complete {
		status = " (depth-limited)"
	}
	fmt.Printf("%s\t%d files\t%d dirs\t%s%s\n", output.FormatFileSize(summary.Size), summary.Files, summary.Directories, summary.Path, status)
}

type duCommandClient interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
	GetFile(fileID string) (*driver.File, error)
}

func summarizeRemoteUsage(client duCommandClient, remotePath string, maxDepth int) (duSummary, error) {
	return summarizeRemoteUsageWithResolver(client, resolver.New(client), remotePath, maxDepth)
}

func summarizeRemoteUsageWithResolver(client duCommandClient, pathResolver *resolver.PathResolver, remotePath string, maxDepth int) (duSummary, error) {
	fileID, isDir, err := pathResolver.ResolvePath(remotePath)
	if err != nil {
		return duSummary{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	if isDir {
		summary, err := calculateDirectoryUsage(client, fileID, remotePath, maxDepth)
		if err != nil {
			return duSummary{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}
		return summary, nil
	}
	file, err := client.GetFile(fileID)
	if err != nil {
		return duSummary{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	return duSummary{Path: remotePath, Size: file.Size, Files: 1, MaxDepth: maxDepth, Complete: true}, nil
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
	addContinueOnErrorFlag(duCmd)
	addBatchFromFileFlag(duCmd)
	rootCmd.AddCommand(duCmd)
}
