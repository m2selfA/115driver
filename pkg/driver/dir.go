package driver

import (
	"fmt"
	"strings"

	"github.com/go-resty/resty/v2"
)

// Mkdir make a new directory which name and parent directory id, return directory id
func (c *Pan115Client) Mkdir(parentID string, name string) (string, error) {
	result := MkdirResp{}
	form := map[string]string{
		"pid":   parentID,
		"cname": name,
	}
	req := c.NewRequest().
		SetFormData(form).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Post(ApiDirAdd)

	err = CheckErr(err, &result, resp)
	if err != nil {
		return "", err
	}
	categoryID := strings.TrimSpace(string(result.CategoryID))
	if categoryID == "" || categoryID == "0" {
		return "", fmt.Errorf("mkdir returned invalid directory id: %w", ErrUnexpected)
	}
	return categoryID, nil
}

// List list all files and directories
func (c *Pan115Client) List(dirID string, opts ...ListOption) (*[]File, error) {
	return c.ListWithLimit(dirID, FileListLimit, opts...)
}

const MaxDirPageLimit = 1150

// ListWithLimit list all files and directories with limit
func (c *Pan115Client) ListWithLimit(dirID string, limit int64, opts ...ListOption) (*[]File, error) {
	if isCalledByAlistV3() {
		return nil, ErrorNotSupportAlist
	}
	if limit <= 0 {
		return nil, fmt.Errorf("file-list limit must be positive: %w", ErrWrongParams)
	}
	if limit > MaxDirPageLimit {
		limit = MaxDirPageLimit
	}

	o := DefaultListOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if len(o.ApiURLs) == 0 {
		return nil, fmt.Errorf("no file-list API endpoints configured")
	}

	var lastErr error
	for _, apiURL := range o.ApiURLs {
		files := make([]File, 0)
		offset := int64(0)
		for {
			req := c.NewRequest().ForceContentType("application/json;charset=UTF-8")
			result, err := GetFiles(req, dirID, WithApiURL(apiURL), WithLimit(limit), WithOffset(offset), withRecordOpenTime(o.recordOpenTime))
			if err != nil {
				lastErr = err
				break
			}
			if int64(result.Count) <= offset {
				return &files, nil
			}
			if int64(result.Offset) != offset {
				lastErr = fmt.Errorf("file-list response offset mismatch: endpoint=%s requested=%d response=%d count=%d", apiURL, offset, result.Offset, result.Count)
				break
			}
			for _, fileInfo := range result.Files {
				files = append(files, *(&File{}).from(&fileInfo))
			}
			nextOffset := int64(result.Offset) + int64(len(result.Files))
			if nextOffset >= int64(result.Count) {
				return &files, nil
			}
			if nextOffset <= offset {
				lastErr = fmt.Errorf("file-list pagination made no progress: endpoint=%s requested=%d response=%d returned=%d count=%d", apiURL, offset, result.Offset, len(result.Files), result.Count)
				break
			}
			offset = nextOffset
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("file-list failed without an endpoint error")
	}
	return nil, lastErr
}

// ListPage list files and directories with page
func (c *Pan115Client) ListPage(dirID string, offset, limit int64, opts ...ListOption) (*[]File, error) {
	if offset < 0 {
		return nil, fmt.Errorf("file-list offset must not be negative: %w", ErrWrongParams)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("file-list limit must be positive: %w", ErrWrongParams)
	}
	if limit > MaxDirPageLimit {
		limit = MaxDirPageLimit
	}
	o := DefaultListOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if len(o.ApiURLs) == 0 {
		return nil, fmt.Errorf("no file-list API endpoints configured")
	}

	var lastErr error
	for _, apiURL := range o.ApiURLs {
		req := c.NewRequest().ForceContentType("application/json;charset=UTF-8")
		result, err := GetFiles(req, dirID, WithApiURL(apiURL), WithLimit(limit), WithOffset(offset), withRecordOpenTime(o.recordOpenTime))
		if err != nil {
			lastErr = err
			continue
		}
		if int64(result.Count) > offset && int64(result.Offset) != offset {
			lastErr = fmt.Errorf("file-list response offset mismatch: endpoint=%s requested=%d response=%d count=%d", apiURL, offset, result.Offset, result.Count)
			continue
		}
		files := make([]File, 0, len(result.Files))
		if int64(result.Count) <= offset {
			return &files, nil
		}
		for _, fileInfo := range result.Files {
			files = append(files, *(&File{}).from(&fileInfo))
		}
		return &files, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("file-list failed without an endpoint error")
	}
	return nil, lastErr
}

func validateFileListResponse(result *FileListResp, dirID string, offset, limit int64) error {
	if result == nil {
		return fmt.Errorf("file-list returned no response: %w", ErrUnexpected)
	}
	if result.Count < 0 || result.Offset < 0 || result.Limit < 0 || result.PageSize < 0 {
		return fmt.Errorf("file-list returned negative pagination metadata: %w", ErrUnexpected)
	}
	if dirID != string(result.CategoryID) {
		return fmt.Errorf("file-list response directory mismatch: requested=%s response=%s: %w", dirID, result.CategoryID, ErrUnexpected)
	}
	if int64(len(result.Files)) > limit {
		return fmt.Errorf("file-list returned %d entries for limit %d: %w", len(result.Files), limit, ErrUnexpected)
	}
	if len(result.Files) > 0 {
		if int64(result.Offset) != offset {
			return fmt.Errorf("file-list response offset mismatch: requested=%d response=%d count=%d: %w", offset, result.Offset, result.Count, ErrUnexpected)
		}
		if int64(result.Offset)+int64(len(result.Files)) > int64(result.Count) {
			return fmt.Errorf("file-list response count is inconsistent: offset=%d returned=%d count=%d: %w", result.Offset, len(result.Files), result.Count, ErrUnexpected)
		}
	}
	for i := range result.Files {
		file := &result.Files[i]
		if strings.TrimSpace(file.FileID) == "" && strings.TrimSpace(string(file.CategoryID)) == "" {
			return fmt.Errorf("file-list result %d has no file or directory id: %w", i, ErrUnexpected)
		}
		if int64(file.Size) < 0 {
			return fmt.Errorf("file-list result %d has negative size: %w", i, ErrUnexpected)
		}
	}
	return nil
}

func GetFiles(req *resty.Request, dirID string, opts ...GetFileOptions) (*FileListResp, error) {
	if req == nil {
		return nil, fmt.Errorf("file-list request is nil: %w", ErrWrongParams)
	}
	if dirID == "" {
		dirID = "0"
	}
	o := DefaultGetFileOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if o.pageSize <= 0 {
		return nil, fmt.Errorf("file-list page size must be positive: %w", ErrWrongParams)
	}
	if o.offset < 0 {
		return nil, fmt.Errorf("file-list offset must not be negative: %w", ErrWrongParams)
	}
	if strings.TrimSpace(o.apiURL) == "" {
		return nil, fmt.Errorf("file-list API URL is empty: %w", ErrWrongParams)
	}
	result := FileListResp{}
	params := map[string]string{
		"aid":              "1",
		"cid":              dirID,
		"o":                o.GetOrder(),
		"asc":              o.GetAsc(),
		"offset":           o.GetOffset(),
		"show_dir":         o.GetshowDir(),
		"limit":            o.GetPageSize(),
		"snap":             "0",
		"natsort":          "0",
		"record_open_time": o.GetRecordOpenTime(),
		"format":           "json",
		"fc_mix":           "0",
	}
	req = req.SetQueryParams(params).SetResult(&result)
	if o.GetApiURL() == ApiFileListByName {
		req.SetQueryParam("o", FileOrderByName)
		req.SetQueryParam("natsort", "1")
	}
	resp, err := req.Get(o.GetApiURL())
	if err = CheckErr(err, &result, resp); err != nil {
		return &FileListResp{}, err
	}
	if err := validateFileListResponse(&result, dirID, o.offset, o.pageSize); err != nil {
		return &FileListResp{}, err
	}
	return &result, nil
}

func (c *Pan115Client) DirName2CID(dir string) (*APIGetDirIDResp, error) {
	result := APIGetDirIDResp{}
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		return &APIGetDirIDResp{CategoryID: IntString("0")}, nil
	}
	req := c.NewRequest().ForceContentType("application/json;charset=UTF-8")
	req.SetQueryParam("path", dir).SetResult(&result)
	resp, err := req.Get(ApiDirName2CID)
	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	categoryID := strings.TrimSpace(string(result.CategoryID))
	if categoryID == "" || categoryID == "0" {
		return nil, fmt.Errorf("directory %q was not resolved to a valid id: %w", dir, ErrNotExist)
	}
	return &result, nil
}
