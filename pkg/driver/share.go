package driver

import (
	"fmt"
	"strconv"
	"strings"
)

type Query func(query *map[string]string)

// QueryLimit set query limit
func QueryLimit(limit int) Query {
	return func(query *map[string]string) {
		(*query)["limit"] = strconv.FormatInt(int64(limit), 10)
	}
}

// QueryOffset set query offset
func QueryOffset(offset int) Query {
	return func(query *map[string]string) {
		(*query)["offset"] = strconv.FormatInt(int64(offset), 10)
	}
}

func validateShareSnapQuery(query map[string]string) (offset, limit int, err error) {
	if strings.TrimSpace(query["share_code"]) == "" {
		return 0, 0, fmt.Errorf("share code is empty: %w", ErrWrongParams)
	}
	if strings.TrimSpace(query["cid"]) == "" {
		return 0, 0, fmt.Errorf("share directory id is empty: %w", ErrWrongParams)
	}
	offset, err = strconv.Atoi(query["offset"])
	if err != nil || offset < 0 {
		return 0, 0, fmt.Errorf("share offset must be a non-negative integer: %w", ErrWrongParams)
	}
	limit, err = strconv.Atoi(query["limit"])
	if err != nil || limit <= 0 {
		return 0, 0, fmt.Errorf("share limit must be a positive integer: %w", ErrWrongParams)
	}
	return offset, limit, nil
}

func validateShareSnapResponse(result *ShareSnapResp, offset, limit int) error {
	if result == nil {
		return fmt.Errorf("share listing returned no response: %w", ErrUnexpected)
	}
	if result.Data.Count < 0 {
		return fmt.Errorf("share listing returned negative count: %w", ErrUnexpected)
	}
	if len(result.Data.List) > limit {
		return fmt.Errorf("share listing returned %d entries for limit %d: %w", len(result.Data.List), limit, ErrUnexpected)
	}
	if len(result.Data.List) > 0 && offset+len(result.Data.List) > result.Data.Count {
		return fmt.Errorf("share listing count is inconsistent: offset=%d returned=%d count=%d: %w", offset, len(result.Data.List), result.Data.Count, ErrUnexpected)
	}
	for i := range result.Data.List {
		item := &result.Data.List[i]
		if strings.TrimSpace(item.FileID) == "" {
			return fmt.Errorf("share listing result %d has no id: %w", i, ErrUnexpected)
		}
		if int64(item.Size) < 0 {
			return fmt.Errorf("share listing result %d has negative size: %w", i, ErrUnexpected)
		}
		if item.IsFile != 0 && item.IsFile != 1 {
			return fmt.Errorf("share listing result %d has invalid file category %d: %w", i, item.IsFile, ErrUnexpected)
		}
	}
	return nil
}

// GetShareSnapWithUA get share snap info with user agent
func (c *Pan115Client) GetShareSnapWithUA(ua, shareCode, receiveCode, dirID string, Queries ...Query) (*ShareSnapResp, error) {
	shareCode = strings.TrimSpace(shareCode)
	if shareCode == "" {
		return nil, fmt.Errorf("share code is empty: %w", ErrWrongParams)
	}
	dirID = strings.TrimSpace(dirID)
	if dirID == "" {
		dirID = "0"
	}
	if isCalledByAlistV3() {
		return nil, ErrorNotSupportAlist
	}
	result := ShareSnapResp{}
	query := map[string]string{
		"share_code":   shareCode,
		"receive_code": receiveCode,
		"cid":          dirID,
		"limit":        "20",
		"asc":          "0",
		"offset":       "0",
		"format":       "json",
	}

	for _, q := range Queries {
		if q != nil {
			q(&query)
		}
	}
	offset, limit, err := validateShareSnapQuery(query)
	if err != nil {
		return nil, err
	}

	req := c.NewRequest().
		SetQueryParams(query).
		SetHeader("referer", BuildShareReferer(shareCode, receiveCode)).
		SetHeader("User-Agent", ua).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)
	resp, err := req.Get(ApiShareSnap)
	if err := CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	if err := validateShareSnapResponse(&result, offset, limit); err != nil {
		return nil, err
	}

	return &result, nil
}

func BuildShareReferer(shareCode, receiveCode string) string {
	return fmt.Sprintf("https://115cdn.com/s/%s?password=%s&", shareCode, receiveCode)
}

// GetShareSnap get share snap info
func (c *Pan115Client) GetShareSnap(shareCode, receiveCode, dirID string, Queries ...Query) (*ShareSnapResp, error) {
	return c.GetShareSnapWithUA("", shareCode, receiveCode, dirID, Queries...)
}
