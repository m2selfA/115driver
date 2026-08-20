package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
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

var lsCmd = &cobra.Command{
	Use:   "ls [remote_path]",
	Short: "List directory contents",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remotePath := "/"
		if len(args) > 0 {
			remotePath = args[0]
		}
		if lsMaxDepth < 0 {
			return &exitError{code: output.ExitArgs, msg: "--max-depth must be >= 0"}
		}
		if lsMaxDepth > 0 && !lsRecursive {
			return &exitError{code: output.ExitArgs, msg: "--max-depth requires --recursive"}
		}

		dirID, err := resolver.ResolveDir(client, remotePath)
		if err != nil {
			return &exitError{code: output.ExitNotFound, msg: err.Error()}
		}

		if lsRecursive {
			offset, limit := normalizeLSPage(lsOffset, lsLimit)
			files, hasMore, depthLimited, err := collectRecursiveLS(client, dirID, remotePath, offset, limit, lsMaxDepth)
			if err != nil {
				return &exitError{code: output.ExitError, msg: err.Error()}
			}
			if jsonOutput {
				printer.PrintSuccess(buildRecursiveLSJSONResponse(remotePath, files, offset, limit, hasMore, lsMaxDepth, depthLimited))
				return nil
			}
			printRecursiveLS(files, lsLong)
			if hasMore {
				fmt.Fprintf(os.Stderr, "Showing %d entries. Use --offset %d to continue.\n", len(files), offset+int64(len(files)))
			}
			return nil
		}

		offset, limit := normalizeLSPage(lsOffset, lsLimit)
		files, err := client.ListPage(dirID, offset, limit)
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}

		jsonFiles := make([]output.JSONFile, 0, len(*files))
		for _, f := range *files {
			jsonFiles = append(jsonFiles, output.FileToJSON(&f))
		}

		if jsonOutput {
			printer.PrintSuccess(buildLSJSONResponse(remotePath, jsonFiles, offset, limit))
			return nil
		}
		if lsLong {
			printer.PrintFileTable(remotePath, jsonFiles)
		} else {
			printer.PrintFileList(remotePath, jsonFiles)
		}
		if notice := buildLSTextPaginationNotice(len(jsonFiles), offset, limit); notice != "" {
			fmt.Fprint(os.Stderr, notice)
		}
		return nil
	},
}

func init() {
	lsCmd.Flags().BoolVarP(&lsLong, "long", "l", false, "Show detailed listing")
	lsCmd.Flags().BoolVarP(&lsRecursive, "recursive", "R", false, "Recursively list descendant entries")
	lsCmd.Flags().IntVar(&lsMaxDepth, "max-depth", 0, "Limit recursive depth (0 = unlimited; direct children are depth 1)")
	lsCmd.Flags().Int64Var(&lsOffset, "offset", 0, "Offset for paginated listing")
	lsCmd.Flags().Int64Var(&lsLimit, "limit", defaultLSLimit, "Max items to list")
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
