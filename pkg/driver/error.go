package driver

import (
	stderrors "errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-resty/resty/v2"
	"github.com/pkg/errors"
)

var (
	ErrorNotSupportAlist = errors.New("not support alist due to privacy risk, please use openlist: https://github.com/OpenListTeam/OpenList")
)

// cookie err
var (
	ErrBadCookie = errors.New("bad cookie")
)

var (
	ErrNotLogin = errors.New("user not login")

	ErrOfflineNoTimes     = errors.New("offline download quota has been used up, you can purchase a VIP experience or upgrade to VIP service to get more quota")
	ErrOfflineInvalidLink = errors.New("invalid download link")
	ErrOfflineTaskExisted = errors.New("offline task existed")

	ErrOrderNotSupport = errors.New("file order not supported")

	ErrPasswordIncorrect    = errors.New("password incorrect")
	ErrLoginTwoStepVerify   = errors.New("requires two-step verification")
	ErrAccountNotBindMobile = errors.New("account not binds mobile")
	ErrCredentialInvalid    = errors.New("credential invalid")
	ErrSessionExited        = errors.New("session exited")

	ErrQrcodeExpired = errors.New("qrcode expired")

	// ErrUnexpected is the fall-back error whose code is not handled.
	ErrUnexpected = errors.New("unexpected error")

	// ErrExist means an item which you want to create is already existed.
	ErrExist = errors.New("target already exists")
	// ErrNotExist means an item which you find is not existed.
	ErrNotExist = errors.New("target does not exist")

	ErrInvalidCursor = errors.New("invalid cursor")

	ErrUploadTooLarge = errors.New("upload reach the limit")

	// ErrUploadNotAllowed means the upload-info endpoint explicitly reports
	// that the current account is not allowed to upload.
	ErrUploadNotAllowed = errors.New("upload is not allowed")

	ErrUploadFailed = errors.New("upload failed")

	// ErrUploadVerificationFailed means 115 rejected a completed upload during
	// server-side verification and requested that the file be uploaded again.
	ErrUploadVerificationFailed = errors.New("upload verification failed; retry upload")

	ErrImportDirectory = errors.New("can not import directory")

	ErrDownloadEmpty = errors.New("can not get download URL")

	ErrDownloadDirectory = errors.New("can not download directory")

	ErrDownloadFileNotExistOrHasDeleted = errors.New("target file does not exist or has deleted")

	ErrDownloadFileTooBig = errors.New("target file is too big to download")

	ErrCyclicCopy = errors.New("cyclic copy")

	ErrCyclicMove = errors.New("cyclic move")

	ErrVideoNotReady = errors.New("video is not ready")

	ErrWrongParams = errors.New("wrong parameters")

	ErrRepeatLogin = errors.New("repeat login")

	ErrFailedToLogin = errors.New("failed to login")

	ErrDoesLoggedOut = errors.New("you have been kicked out by multi-device login management")

	ErrPickCodeNotExist = errors.New("pickcode does not exist")

	ErrSharedInvalid = errors.New("shared link invalid")

	ErrSharedNotFound = errors.New("shared link not found")

	ErrPickCodeIsEmpty = errors.New("empty pickcode")

	ErrPickCodeIsNotExistOrHasDeleted = errors.New("pickcode is not exist or has deleted")

	ErrUploadSH1Invalid = errors.New("userid/filesize/target/pickcode/ invalid")

	ErrUploadSigInvalid = errors.New("sig invalid")

	errMap = map[int]error{
		// Normal errors
		99:     ErrNotLogin,
		990001: ErrNotLogin,
		// Offline errors
		10010: ErrOfflineNoTimes,
		10004: ErrOfflineInvalidLink,
		10008: ErrOfflineTaskExisted,
		// Dir errors
		20004: ErrExist,
		// Label errors
		21003: ErrExist,
		// File errors
		20130827: ErrOrderNotSupport,
		50028:    ErrDownloadFileTooBig,
		70005:    ErrDownloadFileNotExistOrHasDeleted,
		231011:   ErrDownloadFileNotExistOrHasDeleted,
		91002:    ErrCyclicCopy,
		800006:   ErrCyclicMove,
		// Login errors
		40101009: ErrPasswordIncorrect,
		40101010: ErrLoginTwoStepVerify,
		40101017: ErrFailedToLogin,
		40100000: ErrWrongParams,
		40101030: ErrAccountNotBindMobile,
		40101032: ErrCredentialInvalid,
		40101033: ErrRepeatLogin,
		40101035: ErrDoesLoggedOut,
		40101037: ErrSessionExited,
		40101038: ErrRepeatLogin,
		// QRCode errors
		40199002: ErrQrcodeExpired,
		// Params errors
		1001:   ErrWrongParams,
		200900: ErrWrongParams,
		990002: ErrWrongParams,
		// share
		4100009: ErrSharedInvalid,
		4100026: ErrSharedNotFound,
		// pickCode
		50003: ErrPickCodeNotExist,
		50001: ErrPickCodeIsEmpty,
		50015: ErrPickCodeIsNotExistOrHasDeleted,
		// upload SH1
		402: ErrUploadSH1Invalid,
		400: ErrUploadSigInvalid,
	}
)

func GetErr(code int, respBody ...string) error {
	errWithMsg := ErrUnexpected
	if err, found := errMap[code]; found {
		errWithMsg = err
	}
	// if len(respBody) > 0 && errors.Is(ErrUnexpected, errWithMsg) {
	if len(respBody) > 0 {
		bodyRaw := respBody[0]
		readableBody, err := strconv.Unquote(strings.Replace(strconv.Quote(bodyRaw), `\\u`, `\u`, -1))
		if err != nil {
			return errors.Wrap(errWithMsg, bodyRaw)
		}
		return errors.Wrap(errWithMsg, readableBody)
	}
	return errWithMsg
}

type ResultWithErr interface {
	Err(respBody ...string) error
}

// sanitizeHTTPError removes query strings, fragments, and URL userinfo from
// network errors before they cross the driver boundary. 115 request URLs can
// carry credentials, signed tokens, or share receive codes in their query.
func sanitizeHTTPError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if !stderrors.As(err, &urlErr) {
		return err
	}
	sanitized := *urlErr
	parsed, parseErr := url.Parse(urlErr.URL)
	if parseErr != nil {
		sanitized.URL = "[redacted]"
		return &sanitized
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.User = nil
	sanitized.URL = parsed.String()
	if urlErr.Err != nil && urlErr.Err != err {
		sanitized.Err = sanitizeHTTPError(urlErr.Err)
	}
	return &sanitized
}

func validateRestyHTTPStatus(resp *resty.Response) error {
	if resp == nil {
		return ErrUnexpected
	}
	status := resp.StatusCode()
	if status < 200 || status >= 300 {
		return fmt.Errorf("HTTP status %d: %w", status, ErrUnexpected)
	}
	return nil
}

func CheckErr(err error, result ResultWithErr, restyResp *resty.Response) error {
	if err = sanitizeHTTPError(err); err != nil {
		return err
	}
	if result == nil {
		return ErrUnexpected
	}
	if err := validateRestyHTTPStatus(restyResp); err != nil {
		return err
	}
	return result.Err(restyResp.String())
}
