package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var mvDryRun bool

var mvCmd = &cobra.Command{
	Use:   "mv <source_path>... <destination_dir>",
	Short: "Move files or directories into a destination directory",
	Long:  "Move one or more remote files/directories into destination_dir. Moving a directory moves its complete subtree; no --recursive flag is required. --dry-run resolves every source ID and destination mapping without submitting a move request.",
	Args:  transferSourceArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandTransferSourceArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if mvDryRun {
			plan, err := buildMoveOrCopyPlan("move", client, expandedArgs[:len(expandedArgs)-1], expandedArgs[len(expandedArgs)-1])
			if err != nil {
				return err
			}
			printer.PrintSuccess(plan)
			printMoveOrCopyPlan(plan)
			return nil
		}
		return moveOrCopy("move", client, expandedArgs[:len(expandedArgs)-1], expandedArgs[len(expandedArgs)-1], client.Move)
	},
}

func init() {
	addBatchFromFileFlag(mvCmd)
	mvCmd.Flags().BoolVar(&mvDryRun, "dry-run", false, "Plan and validate moves without changing remote data")
	rootCmd.AddCommand(mvCmd)
}

type transferFunc func(dirID string, fileIDs ...string) error

type remotePathResolveClient interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
}

type resolvedRemoteItem struct {
	Path  string
	ID    string
	IsDir bool
}

func resolveUniqueRemoteItems(resolveClient remotePathResolveClient, paths []string) ([]resolvedRemoteItem, error) {
	return resolveUniqueRemoteItemsWithResolver(resolver.New(resolveClient), paths)
}

func resolveUniqueRemoteItemsWithResolver(pathResolver *resolver.PathResolver, paths []string) ([]resolvedRemoteItem, error) {
	items := make([]resolvedRemoteItem, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, remotePath := range paths {
		fileID, isDir, err := pathResolver.ResolvePath(remotePath)
		if err != nil {
			return nil, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}
		if _, exists := seen[fileID]; exists {
			continue
		}
		seen[fileID] = struct{}{}
		items = append(items, resolvedRemoteItem{Path: remotePath, ID: fileID, IsDir: isDir})
	}
	return items, nil
}

func validateRemoteMutationItems(items []resolvedRemoteItem) error {
	for _, item := range items {
		if item.ID == resolver.RootID {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("refusing to mutate remote root path %q", item.Path)}
		}
	}
	return nil
}

func moveOrCopy(action string, resolveClient remotePathResolveClient, srcPaths []string, dstDir string, fn transferFunc) error {
	if len(srcPaths) == 0 {
		return &exitError{code: output.ExitArgs, msg: "at least one source path is required"}
	}
	pathResolver := resolver.New(resolveClient)
	dirID, err := pathResolver.ResolveDir(dstDir)
	if err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve destination directory %s: %v", dstDir, err)}
	}
	items, err := resolveUniqueRemoteItemsWithResolver(pathResolver, srcPaths)
	if err != nil {
		return err
	}
	if err := validateRemoteMutationItems(items); err != nil {
		return err
	}
	fileIDs := make([]string, 0, len(items))
	sources := make([]string, 0, len(items))
	for _, item := range items {
		if item.ID == dirID {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("source %q is the destination directory", item.Path)}
		}
		fileIDs = append(fileIDs, item.ID)
		sources = append(sources, item.Path)
	}
	if err := fn(dirID, fileIDs...); err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}

	printer.PrintSuccess(map[string]interface{}{
		"operation": action, "sources": sources, "destination_dir": dstDir, "file_ids": fileIDs,
	})
	if !jsonOutput {
		verb := "Transferred"
		if action == "copy" {
			verb = "Copied"
		} else if action == "move" {
			verb = "Moved"
		}
		fmt.Printf("%s %d item(s) -> %s\n", verb, len(fileIDs), dstDir)
	}
	return nil
}
