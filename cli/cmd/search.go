package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	searchType   string
	searchSort   string
	searchLimit  int
	searchOffset int
	searchDir    string
	searchAsc    bool
)

var typeMap = map[string]int{
	"all":      0,
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
	Args:  searchArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		keyword := args[0]
		searchDirID := ""
		if searchDir != "" {
			var err error
			searchDirID, err = resolver.ResolveDir(client, searchDir)
			if err != nil {
				return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
			}
		}

		opts := buildSearchOptions(keyword, searchDirID)

		result, err := client.Search(opts)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
		}

		jsonFiles := make([]output.JSONFile, 0, len(result.Files))
		for i := range result.Files {
			jsonFiles = append(jsonFiles, output.FileToJSON(&result.Files[i]))
		}

		response := buildSearchJSONResponse(keyword, searchDir, opts, result, jsonFiles)
		if jsonOutput {
			printer.PrintSuccess(response)
		} else {
			if searchDir != "" {
				fmt.Printf("Found %d results for '%s' in %s:\n\n", result.Count, keyword, searchDir)
			} else {
				fmt.Printf("Found %d results for '%s':\n\n", result.Count, keyword)
			}
			printer.PrintFileTable("", jsonFiles)
			if response["has_more"] == true {
				fmt.Printf("Showing %d results from offset %d. Use --offset %d to continue.\n", len(jsonFiles), opts.Offset, response["next_offset"])
			}
		}
		return nil
	},
}

func searchArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return err
	}
	if searchLimit <= 0 {
		return &exitError{code: output.ExitArgs, msg: "--limit must be > 0"}
	}
	if searchOffset < 0 {
		return &exitError{code: output.ExitArgs, msg: "--offset must be >= 0"}
	}
	if searchType != "" {
		if _, ok := typeMap[searchType]; !ok {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("invalid --type %q; expected all, folder, document, image, video, audio, or archive", searchType)}
		}
	}
	return nil
}

func buildSearchOptions(keyword, directoryID string) *driver.SearchOption {
	opts := &driver.SearchOption{
		SearchValue: keyword,
		Offset:      searchOffset,
		Limit:       searchLimit,
		Cid:         directoryID,
	}
	if searchType != "" {
		opts.Type = typeMap[searchType]
	}
	if searchSort != "" {
		opts.Order = searchSort
	}
	if searchAsc {
		opts.Asc = 1
	}
	return opts
}

func buildSearchJSONResponse(keyword, directory string, opts *driver.SearchOption, result *driver.SearchResult, files []output.JSONFile) map[string]interface{} {
	nextOffset := opts.Offset + len(files)
	hasMore := nextOffset < result.Count
	return map[string]interface{}{
		"keyword":     keyword,
		"directory":   directory,
		"count":       result.Count,
		"files":       files,
		"offset":      opts.Offset,
		"limit":       opts.Limit,
		"has_more":    hasMore,
		"next_offset": nextOffset,
		"order":       result.Order,
		"ascending":   opts.Asc == 1,
	}
}

func init() {
	searchCmd.Flags().StringVarP(&searchType, "type", "t", "", "Filter by type: all, folder, document, image, video, audio, archive")
	searchCmd.Flags().StringVarP(&searchDir, "dir", "d", "", "Scope search to a remote directory using the 115 cid search scope")
	searchCmd.Flags().StringVar(&searchSort, "sort", "", "Sort field (e.g. file_name, file_size, user_ptime)")
	searchCmd.Flags().IntVar(&searchOffset, "offset", 0, "Offset for paginated search results")
	searchCmd.Flags().IntVar(&searchLimit, "limit", 30, "Max results to return")
	searchCmd.Flags().BoolVar(&searchAsc, "asc", false, "Sort ascending instead of the historical default descending order")
	rootCmd.AddCommand(searchCmd)
}
