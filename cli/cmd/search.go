package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	searchType  string
	searchSort  string
	searchLimit int
	searchDir   string
)

var typeMap = map[string]int{
	"folder":   1,
	"document": 2,
	"image":    3,
	"video":    4,
	"audio":    5,
	"archive":  6,
}

var searchCmd = &cobra.Command{
	Use:   "search <keyword>",
	Short: "Search for files",
	Long:  "Search using the 115 search API. --dir passes a directory cid to the server; the CLI does not synthesize a client-side recursive subtree search.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		keyword := args[0]
		searchDirID := ""
		if searchDir != "" {
			var err error
			searchDirID, err = resolver.ResolveDir(client, searchDir)
			if err != nil {
				return &exitError{code: output.ExitNotFound, msg: err.Error()}
			}
		}

		opts := &driver.SearchOption{
			SearchValue: keyword,
			Limit:       searchLimit,
			Cid:         searchDirID,
		}

		if t, ok := typeMap[searchType]; ok {
			opts.Type = t
		}
		if searchSort != "" {
			opts.Order = searchSort
		}

		result, err := client.Search(opts)
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}

		jsonFiles := make([]output.JSONFile, 0, len(result.Files))
		for i := range result.Files {
			jsonFiles = append(jsonFiles, output.FileToJSON(&result.Files[i]))
		}

		if jsonOutput {
			printer.PrintSuccess(map[string]interface{}{
				"keyword":   keyword,
				"directory": searchDir,
				"count":     result.Count,
				"files":     jsonFiles,
			})
		} else {
			if searchDir != "" {
				fmt.Printf("Found %d results for '%s' in %s:\n\n", result.Count, keyword, searchDir)
			} else {
				fmt.Printf("Found %d results for '%s':\n\n", result.Count, keyword)
			}
			printer.PrintFileTable("", jsonFiles)
		}
		return nil
	},
}

func init() {
	searchCmd.Flags().StringVarP(&searchType, "type", "t", "", "Filter by type: folder, document, image, video, audio, archive")
	searchCmd.Flags().StringVarP(&searchDir, "dir", "d", "", "Scope search to a remote directory using the 115 cid search scope")
	searchCmd.Flags().StringVar(&searchSort, "sort", "", "Sort field (e.g. file_name, file_size, user_ptime)")
	searchCmd.Flags().IntVar(&searchLimit, "limit", 30, "Max results to return")
	rootCmd.AddCommand(searchCmd)
}
