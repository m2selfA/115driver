package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

const (
	defaultShareListLimit = 20
	envShareReceiveCode   = "DRIVER115_SHARE_RECEIVE_CODE"
)

var (
	shareReceiveCode string
	shareDirID       = "0"
	shareOffset      int
	shareLimit       = defaultShareListLimit
)

type shareListClient interface {
	GetShareSnap(shareCode, receiveCode, dirID string, queries ...driver.Query) (*driver.ShareSnapResp, error)
}

type shareListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Directory bool   `json:"directory"`
	SHA1      string `json:"sha1,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	Type      string `json:"type,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type shareListResult struct {
	ShareCode  string          `json:"share_code"`
	Directory  string          `json:"dir_id"`
	Title      string          `json:"title,omitempty"`
	Owner      string          `json:"owner,omitempty"`
	Offset     int             `json:"offset"`
	Limit      int             `json:"limit"`
	Count      int             `json:"count"`
	Returned   int             `json:"returned"`
	HasMore    bool            `json:"has_more"`
	NextOffset int             `json:"next_offset"`
	Items      []shareListItem `json:"items"`
}

var shareCmd = &cobra.Command{
	Use:   "share",
	Short: "Inspect 115 share links",
	Args:  cobra.NoArgs,
}

var shareListCmd = &cobra.Command{
	Use:     "ls <share_code>",
	Aliases: []string{"list", "snap"},
	Short:   "List files and directories in a share link",
	Args:    shareListArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		receiveCode, err := resolveShareReceiveCode(cmd, shareReceiveCode)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		result, err := loadShareList(client, args[0], receiveCode, shareDirID, shareOffset, shareLimit)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("List share failed: %v", err)}
		}
		printer.PrintSuccess(result)
		printShareList(result)
		return nil
	},
}

func shareListArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	if strings.TrimSpace(args[0]) == "" {
		return &exitError{code: output.ExitArgs, msg: "share code must not be empty"}
	}
	if _, err := resolveShareReceiveCode(cmd, shareReceiveCode); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if strings.TrimSpace(shareDirID) == "" {
		return &exitError{code: output.ExitArgs, msg: "--dir-id must not be empty"}
	}
	if shareOffset < 0 {
		return &exitError{code: output.ExitArgs, msg: "--offset must be >= 0"}
	}
	if shareLimit <= 0 {
		return &exitError{code: output.ExitArgs, msg: "--limit must be > 0"}
	}
	return nil
}

func resolveShareReceiveCode(cmd *cobra.Command, flagValue string) (string, error) {
	value := strings.TrimSpace(flagValue)
	if cmd != nil && cmd.Flags().Lookup("receive-code") != nil && cmd.Flags().Changed("receive-code") {
		if value == "" {
			return "", fmt.Errorf("--receive-code must not be empty")
		}
		return value, nil
	}
	if value != "" {
		return value, nil
	}
	if envValue := strings.TrimSpace(os.Getenv(envShareReceiveCode)); envValue != "" {
		return envValue, nil
	}
	return "", fmt.Errorf("--receive-code is required (or set %s)", envShareReceiveCode)
}

func loadShareList(listClient shareListClient, shareCode, receiveCode, dirID string, offset, limit int) (shareListResult, error) {
	result := shareListResult{ShareCode: shareCode, Directory: dirID, Offset: offset, Limit: limit}
	response, err := listClient.GetShareSnap(shareCode, receiveCode, dirID, driver.QueryOffset(offset), driver.QueryLimit(limit))
	if err != nil {
		return result, err
	}
	if response == nil {
		return result, fmt.Errorf("share listing returned an empty response")
	}
	result.Title = response.Data.Shareinfo.ShareTitle
	result.Owner = response.Data.Userinfo.UserName
	result.Count = response.Data.Count
	result.Items = make([]shareListItem, 0, len(response.Data.List))
	for _, item := range response.Data.List {
		result.Items = append(result.Items, shareListItem{
			ID: item.FileID, Name: item.FileName, Size: int64(item.Size), Directory: item.IsFile == 0,
			SHA1: item.Sha1, ParentID: item.ParentID, Type: item.Type, UpdatedAt: item.UpdateTime,
		})
	}
	result.Returned = len(result.Items)
	result.NextOffset = offset + result.Returned
	result.HasMore = result.NextOffset < result.Count
	return result, nil
}

func printShareList(result shareListResult) {
	if jsonOutput {
		return
	}
	title := result.Title
	if title == "" {
		title = result.ShareCode
	}
	fmt.Printf("Share %s: %d item(s)\n\n", title, result.Count)
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "TYPE\tSIZE\tID\tNAME")
	for _, item := range result.Items {
		kind := "file"
		size := output.FormatFileSize(item.Size)
		if item.Directory {
			kind = "dir"
			size = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", kind, size, item.ID, item.Name)
	}
	_ = writer.Flush()
	if result.HasMore {
		fmt.Printf("\nShowing %d item(s) from offset %d. Use --offset %d to continue.\n", result.Returned, result.Offset, result.NextOffset)
	}
}

func init() {
	shareListCmd.Flags().StringVar(&shareReceiveCode, "receive-code", "", "Share receive code/password")
	shareListCmd.Flags().StringVar(&shareDirID, "dir-id", "0", "Directory ID inside the share")
	shareListCmd.Flags().IntVar(&shareOffset, "offset", 0, "Offset for paginated share results")
	shareListCmd.Flags().IntVar(&shareLimit, "limit", defaultShareListLimit, "Max share entries to return")
	shareCmd.AddCommand(shareListCmd)
	rootCmd.AddCommand(shareCmd)
}
