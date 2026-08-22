package driver

import (
	"fmt"
	"strconv"
	"strings"
)

// SearchOption defines options for search
type SearchOption struct {
	// Offset for pagination
	Offset int
	// Limit number of results
	Limit int
	// SearchValue search keyword
	SearchValue string
	// Date filter
	Date string
	// Aid area ID
	Aid string
	// Cid category ID
	Cid string
	// PickCode pickcode
	PickCode string
	// Type file type filter 0:all 1:folder 2:document 3:image 4:video 5:audio 6:archive
	Type int
	// CountFolders whether to count folders
	CountFolders int
	// Source source filter
	Source string
	// Star star file only
	Star string
	// Suffix file suffix filter
	Suffix string
	// Order sort field
	Order string
	// Asc ascending order 0:descending 1:ascending
	Asc int
}

// SearchFile represents a file in search results
type SearchFile struct {
	// File ID
	FileID string `json:"fid"`
	// Category ID
	CategoryID IntString `json:"cid"`
	// File name
	Name string `json:"n"`
	// File size
	Size StringInt64 `json:"s"`
	// SHA1 hash
	Sha1 string `json:"sha"`
	// PickCode
	PickCode string `json:"pc"`
	// Is directory
	IsDirectory int `json:"fc"`
	// Is starred file
	IsStar StringInt `json:"m"`
	// Update time
	UpdateTime string `json:"t"`
	// Create time
	CreateTime StringInt64 `json:"tp"`
	// File type icon
	Icon string `json:"ico"`
	// Highlighted file name
	HighlightName string `json:"ns"`
	// File labels
	Labels []*LabelInfo `json:"fl"`
	// Thumbnail URL
	ThumbURL string `json:"u"`
}

// SearchResult represents search results
type SearchResult struct {
	// File list
	Files []File `json:"data"`
	// Total count
	Count int `json:"count"`
	// File count
	FileCount int `json:"file_count"`
	// Folder count
	FolderCount int `json:"folder_count"`
	// Page size
	PageSize int `json:"page_size"`
	// Offset
	Offset int `json:"offset"`
	// Sort field
	Order string `json:"order"`
	// Ascending order
	IsAsc int `json:"is_asc"`
}

func validateSearchOptions(opts *SearchOption) (offset, limit int, err error) {
	offset, limit = 0, 30
	if opts == nil {
		return offset, limit, nil
	}
	if opts.Offset < 0 {
		return 0, 0, fmt.Errorf("search offset must not be negative: %w", ErrWrongParams)
	}
	if opts.Limit < 0 {
		return 0, 0, fmt.Errorf("search limit must not be negative: %w", ErrWrongParams)
	}
	if opts.Type < 0 || opts.Type > 6 {
		return 0, 0, fmt.Errorf("search type must be between 0 and 6: %w", ErrWrongParams)
	}
	if opts.CountFolders < 0 || opts.CountFolders > 1 {
		return 0, 0, fmt.Errorf("search count_folders must be 0 or 1: %w", ErrWrongParams)
	}
	if opts.Asc != 0 && opts.Asc != 1 {
		return 0, 0, fmt.Errorf("search asc must be 0 or 1: %w", ErrWrongParams)
	}
	offset = opts.Offset
	if opts.Limit > 0 {
		limit = opts.Limit
	}
	return offset, limit, nil
}

func validateSearchResponse(result *FileListResp, offset, limit int) error {
	if result == nil {
		return fmt.Errorf("search returned no response: %w", ErrUnexpected)
	}
	if result.Count < 0 || result.Offset < 0 {
		return fmt.Errorf("search returned negative pagination metadata: %w", ErrUnexpected)
	}
	if result.Count > offset && result.Offset != offset {
		return fmt.Errorf("search response offset mismatch: requested=%d response=%d count=%d: %w", offset, result.Offset, result.Count, ErrUnexpected)
	}
	if len(result.Files) > limit {
		return fmt.Errorf("search returned %d entries for limit %d: %w", len(result.Files), limit, ErrUnexpected)
	}
	if len(result.Files) > 0 && offset+len(result.Files) > result.Count {
		return fmt.Errorf("search result count is inconsistent: offset=%d returned=%d count=%d: %w", offset, len(result.Files), result.Count, ErrUnexpected)
	}
	for i := range result.Files {
		file := &result.Files[i]
		if strings.TrimSpace(file.FileID) == "" && strings.TrimSpace(string(file.CategoryID)) == "" {
			return fmt.Errorf("search result %d has no file or directory id: %w", i, ErrUnexpected)
		}
		if int64(file.Size) < 0 {
			return fmt.Errorf("search result %d has negative size: %w", i, ErrUnexpected)
		}
	}
	return nil
}

// Search searches for files using given options
func (c *Pan115Client) Search(opts *SearchOption) (*SearchResult, error) {
	requestedOffset, requestedLimit, err := validateSearchOptions(opts)
	if err != nil {
		return nil, err
	}
	result := FileListResp{}
	params := map[string]string{
		"aid":           "7",
		"cid":           "0",
		"format":        "json",
		"offset":        "0",
		"limit":         "30",
		"search_value":  "",
		"type":          "0",
		"count_folders": "1",
		"o":             "file_name",
		"asc":           "1",
	}

	// Set search parameters
	if opts != nil {
		if opts.Offset >= 0 {
			params["offset"] = strconv.Itoa(opts.Offset)
		}
		if opts.Limit > 0 {
			params["limit"] = strconv.Itoa(opts.Limit)
		}
		if opts.SearchValue != "" {
			params["search_value"] = opts.SearchValue
		}
		if opts.Date != "" {
			params["date"] = opts.Date
		}
		if opts.Aid != "" {
			params["aid"] = opts.Aid
		}
		if opts.Cid != "" {
			params["cid"] = opts.Cid
		}
		if opts.PickCode != "" {
			params["pick_code"] = opts.PickCode
		}
		if opts.Type > 0 {
			params["type"] = strconv.Itoa(opts.Type)
		}
		if opts.CountFolders > 0 {
			params["count_folders"] = strconv.Itoa(opts.CountFolders)
		}
		if opts.Source != "" {
			params["source"] = opts.Source
		}
		if opts.Star != "" {
			params["star"] = opts.Star
		}
		if opts.Suffix != "" {
			params["suffix"] = opts.Suffix
		}
		if opts.Order != "" {
			params["o"] = opts.Order
		}
		params["asc"] = strconv.Itoa(opts.Asc)
	}

	req := c.NewRequest().
		SetQueryParams(params).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Get(ApiFileSearch)
	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	if err := validateSearchResponse(&result, requestedOffset, requestedLimit); err != nil {
		return nil, err
	}

	// Convert results
	searchResult := &SearchResult{
		Count:       result.Count,
		FileCount:   0, // Not available in FileListResp
		FolderCount: 0, // Not available in FileListResp
		PageSize:    result.PageSize,
		Offset:      result.Offset,
		Order:       result.Order,
		IsAsc:       result.IsAsc,
		Files:       make([]File, 0, len(result.Files)),
	}

	for _, fileInfo := range result.Files {
		searchResult.Files = append(searchResult.Files, *(&File{}).from(&fileInfo))
	}

	return searchResult, nil
}
