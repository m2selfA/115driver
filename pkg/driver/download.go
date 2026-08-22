package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	crypto "github.com/SheltonZhu/115driver/pkg/crypto/m115"
	"github.com/go-resty/resty/v2"
)

type FileDownloadUrl struct {
	Client float64 `json:"client"`
	OSSID  string  `json:"oss_id"`
	Url    string  `json:"url"`
	Valid  bool    `json:"-"` // false when API returned false/null
}

// UnmarshalJSON handles both object and bool (false) responses from the API.
func (f *FileDownloadUrl) UnmarshalJSON(b []byte) error {
	// Handle false/null/empty cases
	if len(b) == 0 || string(b) == "false" || string(b) == "null" {
		*f = FileDownloadUrl{}
		return nil
	}
	// Handle object case
	type alias FileDownloadUrl
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*f = FileDownloadUrl(a)
	f.Valid = true
	return nil
}

type DownloadInfo struct {
	FileName string          `json:"file_name"`
	FileSize StringInt64     `json:"file_size"`
	PickCode string          `json:"pick_code"`
	Url      FileDownloadUrl `json:"url"`
	Header   http.Header
}

// Get downloads the file represented by this metadata into a seekable in-memory reader.
func (info *DownloadInfo) Get() (io.ReadSeeker, error) {
	if err := validateDownloadInfo(info, false); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, info.Url.Url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL: %w", ErrUnexpected)
	}
	req.Header = info.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, sanitizeHTTPError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download request returned HTTP status %d: %w", resp.StatusCode, ErrUnexpected)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(body), nil
}

type DownloadData map[string]*DownloadInfo

func selectDownloadInfo(data DownloadData, requestedPickCode string) (*DownloadInfo, error) {
	requestedPickCode = strings.TrimSpace(requestedPickCode)
	if requestedPickCode == "" {
		return nil, ErrPickCodeIsEmpty
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("download metadata is empty: %w", ErrUnexpected)
	}
	if len(data) == 1 {
		for _, info := range data {
			if err := validateDownloadInfo(info, true); err != nil {
				return nil, err
			}
			if responsePickCode := strings.TrimSpace(info.PickCode); responsePickCode != "" && responsePickCode != requestedPickCode {
				return nil, fmt.Errorf("download metadata pickcode mismatch: requested=%q response=%q: %w", requestedPickCode, responsePickCode, ErrUnexpected)
			}
			return info, nil
		}
	}

	var matched *DownloadInfo
	for _, info := range data {
		if info == nil {
			return nil, fmt.Errorf("download metadata contains a nil entry: %w", ErrUnexpected)
		}
		if strings.TrimSpace(info.PickCode) != requestedPickCode {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("download metadata contains multiple entries for pickcode %q: %w", requestedPickCode, ErrUnexpected)
		}
		matched = info
	}
	if matched == nil {
		return nil, fmt.Errorf("download metadata has no entry for pickcode %q: %w", requestedPickCode, ErrUnexpected)
	}
	if err := validateDownloadInfo(matched, true); err != nil {
		return nil, err
	}
	return matched, nil
}

// DownloadWithUA get download info with pickcode and user agent
func (c *Pan115Client) DownloadWithUA(pickCode, ua string) (*DownloadInfo, error) {
	pickCode = strings.TrimSpace(pickCode)
	if pickCode == "" {
		return nil, ErrPickCodeIsEmpty
	}
	key := crypto.GenerateKey()

	result := DownloadResp{}
	params, err := json.Marshal(map[string]string{"pickcode": pickCode})
	if err != nil {
		return nil, err
	}

	data := crypto.Encode(params, key)
	req := c.NewRequest().
		SetQueryParam("t", Now().String()).
		SetFormData(map[string]string{"data": data}).
		ForceContentType("application/json").
		SetResult(&result)
	req = req.SetHeader("User-Agent", ua)
	resp, err := req.Post(ApiDownloadGetUrl)

	if err := CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	bytes, err := crypto.Decode(string(result.EncodedData), key)
	if err != nil {
		return nil, err
	}

	downloadInfo := DownloadData{}
	if err := json.Unmarshal(bytes, &downloadInfo); err != nil {
		return nil, err
	}

	info, err := selectDownloadInfo(downloadInfo, pickCode)
	if err != nil {
		return nil, err
	}
	info.Header = buildDownloadHeaders(sentRequestHeaders(resp), resp.Cookies())
	return info, nil
}

// DownloadWithUAByAndroidAPI get download info with pickcode and user agent
func (c *Pan115Client) DownloadWithUAByAndroidAPI(pickCode string, ua string) (*DownloadInfo, error) {
	pickCode = strings.TrimSpace(pickCode)
	if pickCode == "" {
		return nil, ErrPickCodeIsEmpty
	}
	key := crypto.GenerateKey()

	result := DownloadResp{}
	params, err := json.Marshal(map[string]string{"pick_code": pickCode})
	if err != nil {
		return nil, err
	}

	data := crypto.Encode(params, key)
	req := c.NewRequest().
		SetQueryParam("t", Now().String()).
		SetFormData(map[string]string{"data": data}).
		ForceContentType("application/json").
		SetResult(&result)
	req = req.SetHeader("User-Agent", ua)
	resp, err := req.Post(AndroidApiDownloadGetUrl)

	if err := CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	bytes, err := crypto.Decode(string(result.EncodedData), key)
	if err != nil {
		return nil, err
	}

	infoResp := struct {
		URL string `json:"url"`
	}{}
	if err := json.Unmarshal(bytes, &infoResp); err != nil {
		return nil, err
	}

	info := DownloadInfo{
		Url: FileDownloadUrl{
			Url:   infoResp.URL,
			Valid: strings.TrimSpace(infoResp.URL) != "",
		},
		PickCode: pickCode,
		Header:   buildDownloadHeaders(sentRequestHeaders(resp), resp.Cookies()),
	}
	if err := validateDownloadInfo(&info, false); err != nil {
		return nil, err
	}
	return &info, nil
}

func validateDownloadInfo(info *DownloadInfo, requireKnownSize bool) error {
	if info == nil {
		return ErrUnexpected
	}
	if requireKnownSize && info.FileSize < 0 {
		return ErrDownloadEmpty
	}
	if !info.Url.Valid || strings.TrimSpace(info.Url.Url) == "" {
		return fmt.Errorf("download URL is empty: %w", ErrUnexpected)
	}
	return nil
}

// sentRequestHeaders returns the request headers that were actually sent on
// the wire. resty keeps its own header map (exposed as resp.Request.Header)
// separate from RawRequest.Header — a deep copy made by createHTTPRequest —
// and the empty-UA sentinel is stripped from RawRequest only. Reading the
// raw request headers here is the source of truth for what the peer
// received, and matches the empty-UA handling in applyEmptyUAHandling.
func sentRequestHeaders(resp *resty.Response) http.Header {
	if resp == nil || resp.Request == nil || resp.Request.RawRequest == nil {
		return nil
	}
	return resp.Request.RawRequest.Header
}

// Download get download info with pickcode
func (c *Pan115Client) Download(pickCode string) (*DownloadInfo, error) {
	return c.DownloadWithUA(pickCode, "")
}

func buildDownloadHeaders(requestHeaders http.Header, responseCookies []*http.Cookie) http.Header {
	if requestHeaders == nil {
		requestHeaders = http.Header{}
	}
	headers := requestHeaders.Clone()
	if len(responseCookies) == 0 {
		return headers
	}

	cookies := make([]string, 0, len(responseCookies)+1)
	if existing := strings.TrimSpace(headers.Get("Cookie")); existing != "" {
		cookies = append(cookies, existing)
	}
	for _, cookie := range responseCookies {
		if cookie == nil {
			continue
		}
		cookies = append(cookies, cookie.String())
	}
	if len(cookies) > 0 {
		headers.Set("Cookie", strings.Join(cookies, "; "))
	}
	return headers
}

// SharedDownloadInfo contains the share file metadata returned by 115. Its
// public layout is kept source-compatible with v0.1.4.
type SharedDownloadInfo struct {
	FileID   string      `json:"fid"`
	FileName string      `json:"fn"`
	FileSize StringInt64 `json:"fs"`
	URL      struct {
		URL    string `json:"url"`
		Client int    `json:"client"`
		Desc   any    `json:"desc"`
		Isp    any    `json:"isp"`
		OSSID  string `json:"oss_id"`
		OOID   string `json:"ooid"`
	} `json:"url"`
}

// SharedDownloadRequest adds the request headers/cookies needed to follow a
// signed share URL without changing the legacy SharedDownloadInfo layout.
// Header is deliberately excluded from JSON because it may contain sensitive
// authentication context.
type SharedDownloadRequest struct {
	SharedDownloadInfo
	Header http.Header `json:"-"`
}

// DownloadByShareCode gets legacy share metadata. Call
// DownloadByShareCodeRequest when the signed URL will be followed by the
// caller, because that additive API also carries the required request headers.
func (c *Pan115Client) DownloadByShareCode(shareCode, receiveCode, fileID string) (*SharedDownloadInfo, error) {
	return c.DownloadByShareCodeWithUA("", shareCode, receiveCode, fileID)
}

func (c *Pan115Client) DownloadByShareCodeWithUA(ua, shareCode, receiveCode, fileID string) (*SharedDownloadInfo, error) {
	request, err := c.DownloadByShareCodeRequestWithUA(ua, shareCode, receiveCode, fileID)
	if err != nil {
		return nil, err
	}
	return &request.SharedDownloadInfo, nil
}

// DownloadByShareCodeRequest returns share metadata plus the exact HTTP
// request context required to follow its signed CDN URL.
func (c *Pan115Client) DownloadByShareCodeRequest(shareCode, receiveCode, fileID string) (*SharedDownloadRequest, error) {
	return c.DownloadByShareCodeRequestWithUA("", shareCode, receiveCode, fileID)
}

func (c *Pan115Client) DownloadByShareCodeRequestWithUA(ua, shareCode, receiveCode, fileID string) (*SharedDownloadRequest, error) {
	shareCode = strings.TrimSpace(shareCode)
	fileID = strings.TrimSpace(fileID)
	if shareCode == "" || fileID == "" {
		return nil, fmt.Errorf("share code and file id must be non-empty: %w", ErrWrongParams)
	}
	if isCalledByAlistV3() {
		return nil, ErrorNotSupportAlist
	}
	result := DownloadShareResp{}
	params := map[string]string{
		"share_code":   shareCode,
		"receive_code": receiveCode,
		"file_id":      fileID,
		"dl":           "1",
	}

	req := c.NewRequest().
		SetQueryParams(params).
		ForceContentType("application/json").
		SetHeader("referer", BuildShareReferer(shareCode, receiveCode)).
		SetHeader("User-Agent", ua).
		SetResult(&result)

	resp, err := req.Get(ApiDownloadGetShareUrl)

	if err := CheckErr(err, &result, resp); err != nil {
		return nil, err
	}

	downloadInfo := result.Data
	if strings.TrimSpace(downloadInfo.URL.URL) == "" {
		return nil, fmt.Errorf("share download URL for file %q is empty: %w", fileID, ErrUnexpected)
	}
	if downloadInfo.FileSize < 0 {
		return nil, ErrDownloadEmpty
	}
	if responseID := strings.TrimSpace(downloadInfo.FileID); responseID != "" && responseID != fileID {
		return nil, fmt.Errorf("share download metadata ID mismatch: requested %q, received %q: %w", fileID, responseID, ErrUnexpected)
	}
	return &SharedDownloadRequest{
		SharedDownloadInfo: downloadInfo,
		Header:             buildDownloadHeaders(sentRequestHeaders(resp), resp.Cookies()),
	}, nil
}
