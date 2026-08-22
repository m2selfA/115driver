package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type statCommandClient interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
	Stat(fileID string) (*driver.FileStatInfo, error)
	GetFile(fileID string) (*driver.File, error)
}

type statBatchResult struct {
	Path string          `json:"path"`
	Stat output.JSONStat `json:"stat"`
}

var statCmd = &cobra.Command{
	Use:   "stat <remote_path>...",
	Short: "Show details for one or more files or directories",
	Args:  batchInputArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandBatchInputArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		args = expandedArgs
		if len(args) == 1 {
			jsonStat, err := loadRemoteStat(client, args[0])
			if err != nil {
				return err
			}
			printer.PrintStatTable(jsonStat)
			return nil
		}

		continueOnError := batchContinueOnError(cmd)
		results := make([]statBatchResult, 0, len(args))
		items := make([]batchItemResult, 0, len(args))
		pathResolver := resolver.New(client)
		for i, remotePath := range args {
			jsonStat, err := loadRemoteStatWithResolver(client, pathResolver, remotePath)
			if err != nil {
				items = append(items, failedBatchItem(remotePath, map[string]interface{}{"path": remotePath}, err))
				printBatchItemFailure(i, len(args), "stat "+remotePath, err)
				if !continueOnError {
					break
				}
				continue
			}
			result := statBatchResult{Path: remotePath, Stat: jsonStat}
			results = append(results, result)
			items = append(items, successfulBatchItem(remotePath, result))
		}
		if !jsonOutput {
			for i, result := range results {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("Path: %s\n", result.Path)
				printer.PrintStatTable(result.Stat)
			}
		}
		data := batchResultData(len(args), items, map[string]interface{}{"entries": results})
		if batchFailedCount(items) > 0 {
			return batchIncompleteError("stat batch", len(args), items, data)
		}
		printer.PrintSuccess(data)
		return nil
	},
}

func loadRemoteStat(client statCommandClient, remotePath string) (output.JSONStat, error) {
	return loadRemoteStatWithResolver(client, resolver.New(client), remotePath)
}

func loadRemoteStatWithResolver(client statCommandClient, pathResolver *resolver.PathResolver, remotePath string) (output.JSONStat, error) {
	fileID, _, err := pathResolver.ResolvePath(remotePath)
	if err != nil {
		return output.JSONStat{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	statInfo, err := client.Stat(fileID)
	if err != nil {
		return output.JSONStat{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	jsonStat := output.JSONStat{
		Name:       statInfo.Name,
		IsDir:      statInfo.IsDirectory,
		FileID:     fileID,
		Sha1:       statInfo.Sha1,
		PickCode:   statInfo.PickCode,
		CreateTime: statInfo.CreateTime.Format("2006-01-02 15:04:05"),
		UpdateTime: statInfo.UpdateTime.Format("2006-01-02 15:04:05"),
		FileCount:  statInfo.FileCount,
		DirCount:   statInfo.DirCount,
		Parents:    make([]output.JSONDir, 0, len(statInfo.Parents)),
	}
	if !statInfo.IsDirectory {
		f, err := client.GetFile(fileID)
		if err != nil {
			return output.JSONStat{}, &exitError{code: classifyRemoteError(err, output.ExitError), msg: "Failed to get file details: " + err.Error()}
		}
		jsonStat.Size = f.Size
	}
	for _, p := range statInfo.Parents {
		jsonStat.Parents = append(jsonStat.Parents, output.JSONDir{ID: p.ID, Name: p.Name})
	}
	return jsonStat, nil
}

func init() {
	addContinueOnErrorFlag(statCmd)
	addBatchFromFileFlag(statCmd)
	rootCmd.AddCommand(statCmd)
}
