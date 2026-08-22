package driver

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	crypto "github.com/SheltonZhu/115driver/pkg/crypto/m115"
)

// OfflineTask describe an offline downloading task.
type OfflineTask struct {
	InfoHash     string  `json:"info_hash"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Url          string  `json:"url"`
	AddTime      int64   `json:"add_time"`
	Peers        int64   `json:"peers"`
	RateDownload float64 `json:"rateDownload"`
	Status       int     `json:"status"`
	Percent      float64 `json:"percentDone"`
	UpdateTime   int64   `json:"last_update"`
	LeftTime     int64   `json:"left_time"`
	FileId       string  `json:"file_id"`
	DelFileId    string  `json:"delete_file_id"`
	DirId        string  `json:"wp_path_id"`
	Move         int     `json:"move"`
}

func (t *OfflineTask) IsTodo() bool {
	return t.Status == 0
}

func (t *OfflineTask) IsRunning() bool {
	return t.Status == 1
}

func (t *OfflineTask) IsDone() bool {
	return t.Status == 2
}

func (t *OfflineTask) IsFailed() bool {
	return t.Status == -1
}

func (t *OfflineTask) GetStatus() string {
	if t.IsTodo() {
		return "准备开始离线下载"
	}
	if t.IsDone() {
		return "离线下载完成"
	}
	if t.IsFailed() {
		return "离线下载失败"
	}
	if t.IsRunning() {
		return "离线任务下载中"
	}
	return fmt.Sprintf("未知状态: %d", t.Status)
}

func validateOfflineTaskPage(result *OfflineTaskResp, requestedPage int64) error {
	if result == nil {
		return fmt.Errorf("offline task list returned no response: %w", ErrUnexpected)
	}
	if result.Total < 0 || result.Count < 0 || result.PageRow < 0 || result.PageCount < 0 || result.Page < 0 {
		return fmt.Errorf("offline task list returned negative pagination metadata: %w", ErrUnexpected)
	}
	if result.Page > 0 && result.Page != requestedPage {
		return fmt.Errorf("offline task list returned page %d for requested page %d: %w", result.Page, requestedPage, ErrUnexpected)
	}
	if result.Page > 0 && result.PageCount > 0 && result.Page > result.PageCount {
		return fmt.Errorf("offline task list returned page %d beyond page_count %d: %w", result.Page, result.PageCount, ErrUnexpected)
	}
	for i, task := range result.Tasks {
		if task == nil {
			return fmt.Errorf("offline task list returned null task at index %d: %w", i, ErrUnexpected)
		}
	}
	return nil
}

// ListOfflineTask list tasks
func (c *Pan115Client) ListOfflineTask(page int64) (OfflineTaskResp, error) {
	result := OfflineTaskResp{}
	if page <= 0 {
		return result, fmt.Errorf("offline task page must be positive: %w", ErrWrongParams)
	}
	if isCalledByAlistV3() {
		return result, ErrorNotSupportAlist
	}
	req := c.NewRequest().
		SetQueryParam("page", strconv.FormatInt(page, 10)).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Post(ApiListOfflineUrl)

	if err := CheckErr(err, &result, resp); err != nil {
		return OfflineTaskResp{}, err
	}
	if err := validateOfflineTaskPage(&result, page); err != nil {
		return OfflineTaskResp{}, err
	}
	return result, nil
}

// AddOfflineTaskURIs adds offline tasks by download URIs.
// supports http, ed2k, magent
func (c *Pan115Client) AddOfflineTaskURIs(uris []string, saveDirID string, opts ...OfflineOption) (hashes []string, err error) {
	if isCalledByAlistV3() {
		return nil, ErrorNotSupportAlist
	}
	opt := DefaultOfflineOptions()

	for _, o := range opts {
		if o != nil {
			o(&opt)
		}
	}
	count := len(uris)
	if count == 0 {
		return
	}
	for _, uri := range uris {
		if strings.TrimSpace(uri) == "" {
			return nil, fmt.Errorf("offline task URL is empty: %w", ErrWrongParams)
		}
	}

	if c.UserID <= 0 {
		userInfo, err := c.GetUser()
		if err != nil {
			return nil, err
		}
		c.UserID = userInfo.UserID
	}

	key := crypto.GenerateKey()

	result := DownloadResp{}

	params := map[string]string{
		"ac":         "add_task_urls",
		"wp_path_id": saveDirID,
		"app_ver":    opt.appVer,
		"uid":        strconv.FormatInt(c.UserID, 10),
	}
	for i, uri := range uris {
		key := fmt.Sprintf("url[%d]", i)
		params[key] = uri
	}
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	data := crypto.Encode(paramsBytes, key)
	req := c.NewRequest().
		SetQueryParam("t", Now().String()).
		SetFormData(map[string]string{"data": data}).
		ForceContentType("application/json").
		SetResult(&result)

	resp, err := req.Post(ApiAddOfflineUrl)

	if err := CheckErr(err, &result, resp); err != nil {
		return nil, err
	}

	bytes, err := crypto.Decode(string(result.EncodedData), key)
	if err != nil {
		return nil, err
	}

	taskInfos := offlineAddURLWireResponse{}
	if err := json.Unmarshal(bytes, &taskInfos); err != nil {
		return nil, err
	}

	return collectOfflineTaskHashes(taskInfos, count)
}

// DeleteOfflineTasks deletes tasks.
func (c *Pan115Client) DeleteOfflineTasks(hashes []string, deleteFiles bool) error {
	if isCalledByAlistV3() {
		return ErrorNotSupportAlist
	}
	if len(hashes) == 0 {
		return fmt.Errorf("offline task hashes are empty: %w", ErrWrongParams)
	}
	form := url.Values{}
	for _, hash := range hashes {
		hash = strings.TrimSpace(hash)
		if hash == "" {
			return fmt.Errorf("offline task hash is empty: %w", ErrWrongParams)
		}
		form.Add("hash", hash)
	}

	form.Set("flag", "0")
	if deleteFiles {
		form.Set("flag", "1")
	}

	result := MkdirResp{}
	req := c.NewRequest().
		SetFormDataFromValues(form).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Post(ApiDelOfflineUrl)
	return CheckErr(err, &result, resp)
}

// ClearOfflineTasks deletes tasks.
func (c *Pan115Client) ClearOfflineTasks(clearFlag int64) error {
	form := url.Values{}
	form.Set("flag", strconv.FormatInt(int64(clearFlag), 10))

	result := MkdirResp{}
	req := c.NewRequest().
		SetFormDataFromValues(form).
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8")

	resp, err := req.Post(ApiClearOfflineUrl)
	return CheckErr(err, &result, resp)
}

type OfflineAddUrlResponse struct {
	BasicResp
	Result []OfflineTaskResponse `json:"result"`
}
type OfflineTaskResponse struct {
	InfoHash string `json:"info_hash"`
	Url      string `json:"url"`
}

// offlineTaskWireResponse carries response-only diagnostics used to prove that
// every requested URI was accepted. Keeping these fields private preserves the
// v0.1.4 OfflineTaskResponse source layout for library callers.
type offlineTaskWireResponse struct {
	OfflineTaskResponse
	Name      string `json:"name"`
	State     *bool  `json:"state"`
	ErrCode   int    `json:"errcode"`
	ErrorCode int    `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	Error     string `json:"error"`
}

type offlineAddURLWireResponse struct {
	BasicResp
	Result []offlineTaskWireResponse `json:"result"`
}

func collectOfflineTaskHashes(resp offlineAddURLWireResponse, expected int) ([]string, error) {
	if len(resp.Result) != expected {
		return nil, fmt.Errorf("offline add returned %d results for %d URLs: %w", len(resp.Result), expected, ErrUnexpected)
	}
	hashes := make([]string, expected)
	for i, task := range resp.Result {
		code := findNonZero(task.ErrCode, task.ErrorCode)
		if task.State != nil && !*task.State {
			if code != 0 {
				return nil, fmt.Errorf("offline add result %d failed: %w", i, GetErr(code))
			}
			message := strings.TrimSpace(task.ErrorMsg)
			if message == "" {
				message = strings.TrimSpace(task.Error)
			}
			if message != "" {
				return nil, fmt.Errorf("offline add result %d failed: %s: %w", i, message, ErrUnexpected)
			}
			return nil, fmt.Errorf("offline add result %d reported failure: %w", i, ErrUnexpected)
		}
		hash := strings.TrimSpace(task.InfoHash)
		if hash == "" {
			if code != 0 {
				return nil, fmt.Errorf("offline add result %d has no info hash: %w", i, GetErr(code))
			}
			return nil, fmt.Errorf("offline add result %d has no info hash: %w", i, ErrUnexpected)
		}
		hashes[i] = hash
	}
	return hashes, nil
}

type OfflineTaskResp struct {
	BasicResp
	Total     int64          `json:"total"`
	Count     int64          `json:"count"`
	PageRow   int64          `json:"page_row"`
	PageCount int64          `json:"page_count"`
	Page      int64          `json:"page"`
	Quota     int64          `json:"quota"`
	Tasks     []*OfflineTask `json:"tasks"`
}
