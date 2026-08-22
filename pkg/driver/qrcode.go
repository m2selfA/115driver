package driver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-resty/resty/v2"
	qrcode "github.com/skip2/go-qrcode"
)

type QRCodeSession struct {
	// The raw data of QRCode, caller should use third-party tools/libraries
	// to convert it into QRCode matrix or image.
	QrcodeContent string `json:"qrcode"`
	Sign          string `json:"sign"`
	Time          int64  `json:"time"`
	UID           string `json:"uid"`
}

// QRCode get QRCode matrix or image.
func (s *QRCodeSession) QRCode() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("qrcode session is nil: %w", ErrWrongParams)
	}
	if strings.TrimSpace(s.QrcodeContent) == "" {
		return nil, fmt.Errorf("qrcode content is empty: %w", ErrUnexpected)
	}
	return qrcode.Encode(s.QrcodeContent, qrcode.Medium, 256)
}

// QRCodeByApi get QRCode matrix or image by api.
func (s *QRCodeSession) QRCodeByApi() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("qrcode session is nil: %w", ErrWrongParams)
	}
	if strings.TrimSpace(s.UID) == "" {
		return nil, fmt.Errorf("qrcode session uid is empty: %w", ErrWrongParams)
	}
	return fetchQRCodeImage(resty.New(), fmt.Sprintf(ApiQrcodeImage, s.UID))
}

func fetchQRCodeImage(client *resty.Client, rawURL string) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("qrcode HTTP client is nil: %w", ErrWrongParams)
	}
	resp, err := client.R().Get(rawURL)
	if err != nil {
		return nil, sanitizeHTTPError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("qrcode image request returned no response: %w", ErrUnexpected)
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("qrcode image request returned HTTP status %d: %w", resp.StatusCode(), ErrUnexpected)
	}
	body := resp.Body()
	if len(body) == 0 {
		return nil, fmt.Errorf("qrcode image response is empty: %w", ErrUnexpected)
	}
	return append([]byte(nil), body...), nil
}

// QRCodeStart starts a QRCode login session.
func (c *Pan115Client) QRCodeStart() (*QRCodeSession, error) {
	result := QRCodeTokenResp{}
	resp, err := c.NewRequest().
		SetResult(&result).
		ForceContentType("application/json;charset=UTF-8").
		Get(ApiQrcodeToken)

	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.Data.UID) == "" || strings.TrimSpace(result.Data.Sign) == "" || result.Data.Time <= 0 {
		return nil, fmt.Errorf("qrcode token returned incomplete session data: %w", ErrUnexpected)
	}
	return &result.Data, nil
}

// QRCodeLogin logins user through QRCode with web app.
// You SHOULD call this method ONLY when `QRCodeStatus.IsAllowed()` is true.
func (c *Pan115Client) QRCodeLogin(s *QRCodeSession) (*Credential, error) {
	return c.QRCodeLoginWithApp(s, LoginAppWeb)
}

type LoginApp string

const (
	LoginAppWeb     LoginApp = "web"
	LoginAppAndroid LoginApp = "android"
	LoginAppIOS     LoginApp = "ios"
	// LoginAppLinux      LoginApp = "linux"   // disabled
	// LoginAppMac        LoginApp = "mac"     // disabled
	// LoginAppWindows    LoginApp = "windows" // disabled
	LoginAppTV         LoginApp = "tv"
	LoginAppAlipayMini LoginApp = "alipaymini"
	LoginAppWechatMini LoginApp = "wechatmini"
	LoginQAppAndroid   LoginApp = "qandroid"
)

// QRCodeLoginWithApp logins user through QRCode with specified app.
// You SHOULD call this method ONLY when `QRCodeStatus.IsAllowed()` is true.
func (c *Pan115Client) QRCodeLoginWithApp(s *QRCodeSession, app LoginApp) (*Credential, error) {
	if s == nil {
		return nil, fmt.Errorf("qrcode session is nil: %w", ErrWrongParams)
	}
	if strings.TrimSpace(s.UID) == "" {
		return nil, fmt.Errorf("qrcode session uid is empty: %w", ErrWrongParams)
	}
	result := QRCodeLoginResp{}
	req := c.NewRequest().
		SetFormData(map[string]string{
			"account": s.UID,
			"app":     string(app),
		}).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)
	resp, err := req.Post(fmt.Sprintf(ApiQrcodeLoginWithApp, app))
	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	credential := &result.Data.Credential
	if strings.TrimSpace(credential.UID) == "" || strings.TrimSpace(credential.CID) == "" || strings.TrimSpace(credential.SEID) == "" {
		return nil, fmt.Errorf("qrcode login returned incomplete credential: %w", ErrUnexpected)
	}
	return credential, nil
}

type QRCodeStatus struct {
	Msg     string `json:"msg"`
	Status  int    `json:"status"`
	Version string `json:"version"`
}

func (s *QRCodeStatus) IsWaiting() bool {
	return s != nil && s.Status == 0
}

func (s *QRCodeStatus) IsScanned() bool {
	return s != nil && s.Status == 1
}

func (s *QRCodeStatus) IsAllowed() bool {
	return s != nil && s.Status == 2
}

func (s *QRCodeStatus) IsExpired() bool {
	return s != nil && s.Status == -1
}

func (s *QRCodeStatus) IsCanceled() bool {
	return s != nil && s.Status == -2
}

/*
QRCodeStatus represents the status of a QRCode session.

There are 5 possible status values:
- Waiting (0)
- Scanned (1)
- Allowed (2)
- Expired (-1)
- Canceled (-2)
*/
const qrCodeStatusMissing = -1 << 30

func (c *Pan115Client) QRCodeStatus(s *QRCodeSession) (*QRCodeStatus, error) {
	if s == nil {
		return nil, fmt.Errorf("qrcode session is nil: %w", ErrWrongParams)
	}
	if strings.TrimSpace(s.UID) == "" || strings.TrimSpace(s.Sign) == "" || s.Time <= 0 {
		return nil, fmt.Errorf("qrcode session is incomplete: %w", ErrWrongParams)
	}
	// Seed an impossible status so both a missing data field and JSON null remain
	// distinguishable from the documented waiting status (0) without changing
	// QRCodeStatusResp.Data from its v0.1.4 value type.
	result := QRCodeStatusResp{Data: QRCodeStatus{Status: qrCodeStatusMissing}}
	req := c.NewRequest().
		SetQueryParams(map[string]string{
			"uid":  s.UID,
			"time": strconv.FormatInt(s.Time, 10),
			"sign": s.Sign,
			"_":    Now().String(),
		}).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)

	resp, err := req.Get(ApiQrcodeStatus)
	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	switch result.Data.Status {
	case -2, -1, 0, 1, 2:
		return &result.Data, nil
	default:
		return nil, fmt.Errorf("qrcode status response has unknown status %d: %w", result.Data.Status, ErrUnexpected)
	}
}
