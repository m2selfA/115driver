package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var lsLong bool
var lsOffset int64
var lsLimit int64
var lsRecursive bool
var lsMaxDepth int

const (
	defaultLSLimit int64 = 100
	maxLSLimit     int64 = 500
)

type lsListingResult struct {
	Path           string
	Files          []output.JSONFile
	RecursiveFiles []recursiveLSFile
	Recursive      bool
	Offset         int64
	Limit          int64
	HasMore        bool
	MaxDepth       int
	DepthLimited   bool
}

type lsBatchResult struct {
	Path    string                 `json:"path"`
	Listing map[string]interface{} `json:"listing"`
}

var lsCmd = &cobra.Command{
	Use:   "ls [remote_path]...",
	Short: "List one or more directory contents",
	Args:  lsInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLSCommand(client, cmd, args)
	},
}

func lsInputArgs(cmd *cobra.Command, _ []string) error {
	if lsMaxDepth < 0 {
		return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
	}
	if lsMaxDepth > 0 && !lsRecursive {
		return &exitError{code: output.ExitArgs, msg: "--max-depth requires --recursive"}
	}
	_, err := batchFromFile(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return nil
}

func runLSCommand(listClient remotePathResolveClient, cmd *cobra.Command, args []string) error {
	if lsMaxDepth < 0 {
		return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
	}
	if lsMaxDepth > 0 && !lsRecursive {
		return &exitError{code: output.ExitArgs, msg: "--max-depth requires --recursive"}
	}
	expandedArgs, err := expandLSInputArgs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	args = expandedArgs
	pathResolver := resolver.New(listClient)

	if len(args) == 1 {
		result, err := loadLSListing(listClient, pathResolver, args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			printer.PrintSuccess(result.jsonData())
			return nil
		}
		printLSListing(result, false)
		return nil
	}

	continueOnError := batchContinueOnError(cmd)
	results := make([]lsListingResult, 0, len(args))
	jsonResults := make([]lsBatchResult, 0, len(args))
	items := make([]batchItemResult, 0, len(args))
	for i, remotePath := range args {
		result, err := loadLSListing(listClient, pathResolver, remotePath)
		if err != nil {
			items = append(items, failedBatchItem(remotePath, map[string]interface{}{"path": remotePath}, err))
			printBatchItemFailure(i, len(args), "ls "+remotePath, err)
			if !continueOnError {
				break
			}
			continue
		}
		entry := lsBatchResult{Path: remotePath, Listing: result.jsonData()}
		results = append(results, result)
		jsonResults = append(jsonResults, entry)
		items = append(items, successfulBatchItem(remotePath, entry))
	}
	if !jsonOutput {
		for i, result := range results {
			if i > 0 {
				fmt.Println()
			}
			printLSListing(result, true)
		}
	}
	data := batchResultData(len(args), items, map[string]interface{}{"entries": jsonResults})
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("ls batch", len(args), items, data)
	}
	printer.PrintSuccess(data)
	return nil
}

func expandLSInputArgs(cmd *cobra.Command, args []string) ([]string, error) {
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return nil, err
	}
	if fromFile == "" {
		if len(args) == 0 {
			return []string{"/"}, nil
		}
		return append([]string(nil), args...), nil
	}
	return expandBatchInputArgs(cmd, args)
}

func loadLSListing(listClient remotePathResolveClient, pathResolver *resolver.PathResolver, remotePath string) (lsListingResult, error) {
	result := lsListingResult{Path: remotePath, Recursive: lsRecursive, MaxDepth: lsMaxDepth}
	dirID, err := pathResolver.ResolveDir(remotePath)
	if err != nil {
		return result, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	result.Offset, result.Limit = normalizeLSPage(lsOffset, lsLimit)
	if lsRecursive {
		result.RecursiveFiles, result.HasMore, result.DepthLimited, err = collectRecursiveLS(listClient, dirID, remotePath, result.Offset, result.Limit, lsMaxDepth)
		if err != nil {
			return result, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}
		return result, nil
	}

	files, err := listClient.ListPage(dirID, result.Offset, result.Limit, driver.WithRecordOpenTime(false))
	if err != nil {
		return result, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	if files == nil {
		return result, &exitError{code: output.ExitError, msg: "list directory returned an empty response"}
	}
	result.Files = make([]output.JSONFile, 0, len(*files))
	for _, file := range *files {
		result.Files = append(result.Files, output.FileToJSON(&file))
	}
	result.HasMore = result.Limit > 0 && int64(len(result.Files)) == result.Limit
	return result, nil
}

func (result lsListingResult) jsonData() map[string]interface{} {
	if result.Recursive {
		return buildRecursiveLSJSONResponse(result.Path, result.RecursiveFiles, result.Offset, result.Limit, result.HasMore, result.MaxDepth, result.DepthLimited)
	}
	return buildLSJSONResponse(result.Path, result.Files, result.Offset, result.Limit)
}

func printLSListing(result lsListingResult, includePath bool) {
	if includePath && (result.Recursive || !lsLong) {
		fmt.Printf("Path: %s\n", result.Path)
	}
	if result.Recursive {
		printRecursiveLS(result.RecursiveFiles, lsLong)
		if result.HasMore {
			printLSPaginationNotice(result.Path, len(result.RecursiveFiles), result.Offset, includePath)
		}
		return
	}
	if lsLong {
		printer.PrintFileTable(result.Path, result.Files)
	} else {
		printer.PrintFileList(result.Path, result.Files)
	}
	if notice := buildLSTextPaginationNotice(len(result.Files), result.Offset, result.Limit); notice != "" {
		if includePath {
			fmt.Fprintf(os.Stderr, "%s: %s", result.Path, notice)
		} else {
			fmt.Fprint(os.Stderr, notice)
		}
	}
}

func printLSPaginationNotice(path string, fileCount int, offset int64, includePath bool) {
	if includePath {
		fmt.Fprintf(os.Stderr, "%s: Showing %d entries. Use --offset %d to continue.\n", path, fileCount, offset+int64(fileCount))
		return
	}
	fmt.Fprintf(os.Stderr, "Showing %d entries. Use --offset %d to continue.\n", fileCount, offset+int64(fileCount))
}

func init() {
	lsCmd.Flags().BoolVarP(&lsLong, "long", "l", false, "Show detailed listing")
	lsCmd.Flags().BoolVarP(&lsRecursive, "recursive", "R", false, "Recursively list descendant entries")
	lsCmd.Flags().IntVar(&lsMaxDepth, "max-depth", 0, "Limit recursive depth (0 = unlimited; direct children are depth 1)")
	lsCmd.Flags().Int64Var(&lsOffset, "offset", 0, "Offset for paginated listing")
	lsCmd.Flags().Int64Var(&lsLimit, "limit", defaultLSLimit, "Max items to list")
	addContinueOnErrorFlag(lsCmd)
	addBatchFromFileFlag(lsCmd)
	rootCmd.AddCommand(lsCmd)
}

type recursiveLSFile struct {
	output.JSONFile
	RelativePath string `json:"relative_path"`
	RemotePath   string `json:"path"`
	Depth        int    `json:"depth"`
}

func collectRecursiveLS(client remoteTreeListClient, rootID, rootPath string, offset, limit int64, maxDepth int) ([]recursiveLSFile, bool, bool, error) {
	offset, limit = normalizeLSPage(offset, limit)
	files := make([]recursiveLSFile, 0, limit)
	var seen int64
	walkResult, err := walkRemoteTree(client, rootID, rootPath, maxDepth, func(entry remoteWalkEntry) (bool, error) {
		if seen < offset {
			seen++
			return false, nil
		}
		seen++
		file := entry.File
		files = append(files, recursiveLSFile{
			JSONFile: output.FileToJSON(&file), RelativePath: entry.RelativePath, RemotePath: entry.RemotePath, Depth: entry.Depth,
		})
		return int64(len(files)) > limit, nil
	})
	if err != nil {
		return nil, false, false, err
	}
	hasMore := int64(len(files)) > limit || walkResult.StoppedEarly
	if int64(len(files)) > limit {
		files = files[:limit]
	}
	return files, hasMore, walkResult.DepthLimited, nil
}

func buildRecursiveLSJSONResponse(path string, files []recursiveLSFile, offset, limit int64, hasMore bool, maxDepth int, depthLimited bool) map[string]interface{} {
	return map[string]interface{}{
		"path": path, "files": files, "recursive": true, "max_depth": maxDepth, "depth_limited": depthLimited,
		"offset": offset, "limit": limit, "has_more": hasMore, "next_offset": offset + int64(len(files)),
	}
}

func printRecursiveLS(files []recursiveLSFile, long bool) {
	if !long {
		for _, file := range files {
			name := file.RelativePath
			if file.IsDir {
				name += "/"
			}
			fmt.Println(name)
		}
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tSIZE\tTYPE\tMODIFIED")
	fmt.Fprintln(w, "----\t----\t----\t--------")
	for _, file := range files {
		name := file.RelativePath
		typ := "file"
		if file.IsDir {
			typ = "dir"
			name += "/"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, output.FormatFileSize(file.Size), typ, file.UpdateTime)
	}
	_ = w.Flush()
}

func normalizeLSPage(offset, limit int64) (int64, int64) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultLSLimit
	}
	if limit > maxLSLimit {
		limit = maxLSLimit
	}
	return offset, limit
}

func buildLSJSONResponse(path string, files []output.JSONFile, offset, limit int64) map[string]interface{} {
	hasMore := limit > 0 && int64(len(files)) == limit
	return map[string]interface{}{
		"path":        path,
		"files":       files,
		"offset":      offset,
		"limit":       limit,
		"has_more":    hasMore,
		"next_offset": offset + int64(len(files)),
	}
}

func buildLSTextPaginationNotice(fileCount int, offset, limit int64) string {
	if limit <= 0 || int64(fileCount) < limit {
		return ""
	}
	nextOffset := offset + int64(fileCount)
	return fmt.Sprintf("Showing %d entries. Use --offset %d to continue.\n", fileCount, nextOffset)
}
