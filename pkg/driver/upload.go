package driver

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	hash "github.com/SheltonZhu/115driver/pkg/crypto"
	cipher "github.com/SheltonZhu/115driver/pkg/crypto/ec115"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/pkg/errors"
)

// GetDigestResult get digest of file or stream
func (c *Pan115Client) GetDigestResult(r io.Reader) (*hash.DigestResult, error) {
	d := hash.DigestResult{}
	return &d, hash.Digest(r, &d)
}

// GetUploadEndpoint get upload endPoint
func (c *Pan115Client) GetUploadEndpoint(endpoint *UploadEndpointResp) error {
	if endpoint == nil {
		return fmt.Errorf("upload endpoint output is nil: %w", ErrWrongParams)
	}
	req := c.NewRequest().
		ForceContentType("application/json;charset=UTF-8").
		SetResult(endpoint)
	resp, err := req.Get(ApiGetUploadEndpoint)
	if err != nil {
		return sanitizeHTTPError(err)
	}
	if err := validateRestyHTTPStatus(resp); err != nil {
		return err
	}
	if strings.TrimSpace(endpoint.Endpoint) == "" {
		return fmt.Errorf("upload endpoint response is empty: %w", ErrUnexpected)
	}
	return nil
}

// GetUploadInfo gets the account metadata required for uploads. The refresh is
// serialized because parallel upload workers share one Pan115Client and publish
// UserID/Userkey/UploadMetaInfo on that client.
func (c *Pan115Client) GetUploadInfo() error {
	c.uploadInfoMu.Lock()
	defer c.uploadInfoMu.Unlock()
	return c.getUploadInfoLocked()
}

func validateUploadInfoResponse(result UploadInfoResp) error {
	if result.UserID <= 0 {
		return fmt.Errorf("upload metadata response is missing a valid user_id")
	}
	if strings.TrimSpace(result.Userkey) == "" {
		return fmt.Errorf("upload metadata response is missing userkey")
	}
	return nil
}

func (c *Pan115Client) getUploadInfoLocked() error {
	result := UploadInfoResp{}
	req := c.NewRequest().
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)
	resp, err := req.Post(ApiUploadInfo)
	if err = CheckErr(err, &result, resp); err != nil {
		return err
	}
	if err := validateUploadInfoResponse(result); err != nil {
		return err
	}
	if c.UserID != 0 && c.UserID != result.UserID {
		return fmt.Errorf("upload metadata account mismatch: authenticated user %d, upload metadata user %d", c.UserID, result.UserID)
	}
	if c.UserID == 0 {
		c.UserID = result.UserID
	}
	c.Userkey = result.Userkey
	c.UploadMetaInfo = &result.UploadMetaInfo
	return nil
}

func (c *Pan115Client) uploadInfoReadyLocked() bool {
	return c.UserID != 0 && len(c.Userkey) > 0 && c.UploadMetaInfo != nil
}

func (c *Pan115Client) uploadPermissionErrorLocked() error {
	if c.UploadMetaInfo == nil || c.UploadMetaInfo.UploadAllowed {
		return nil
	}
	message := strings.TrimSpace(c.UploadMetaInfo.UploadAllowedMsg)
	if message == "" {
		return ErrUploadNotAllowed
	}
	return fmt.Errorf("%w: %s", ErrUploadNotAllowed, message)
}

// UploadAvailable checks and prepares upload metadata. The check-and-fetch
// sequence is one critical section so concurrent batch workers perform at most
// one successful initialization request and all observe fully published fields.
func (c *Pan115Client) UploadAvailable() (bool, error) {
	c.uploadInfoMu.Lock()
	defer c.uploadInfoMu.Unlock()
	if c.uploadInfoReadyLocked() {
		if err := c.uploadPermissionErrorLocked(); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := c.getUploadInfoLocked(); err != nil {
		return false, err
	}
	if !c.uploadInfoReadyLocked() {
		return false, fmt.Errorf("upload metadata remains incomplete after refresh")
	}
	if err := c.uploadPermissionErrorLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// UploadFastOrByOSS Upload By OSS when unable to rapid upload file
// Deprecated: As of v1.0.22, this function simply calls [RapidUploadOrByOSS].
func (c *Pan115Client) UploadFastOrByOSS(dirID, fileName string, fileSize int64, r io.ReadSeeker) error {
	return c.RapidUploadOrByOSS(dirID, fileName, fileSize, r)
}

func validateUploadSourceSize(fileSize, actualSize int64) error {
	if fileSize < 0 {
		return fmt.Errorf("upload file size is negative: %w", ErrWrongParams)
	}
	if fileSize != actualSize {
		return fmt.Errorf("upload file size mismatch: declared=%d actual=%d: %w", fileSize, actualSize, ErrWrongParams)
	}
	return nil
}

// RapidUploadOrByOSS Upload By OSS when unable to rapid upload file
func (c *Pan115Client) RapidUploadOrByOSS(dirID, fileName string, fileSize int64, r io.ReadSeeker) error {
	if r == nil {
		return fmt.Errorf("upload reader is nil: %w", ErrWrongParams)
	}
	if fileSize < 0 {
		return fmt.Errorf("upload file size is negative: %w", ErrWrongParams)
	}
	var (
		err      error
		digest   *hash.DigestResult
		fastInfo *UploadInitResp
	)

	if ok, err := c.UploadAvailable(); err != nil || !ok {
		return err
	}
	if fileSize > c.UploadMetaInfo.SizeLimit {
		return ErrUploadTooLarge
	}
	if digest, err = c.GetDigestResult(r); err != nil {
		return err
	}
	if err := validateUploadSourceSize(fileSize, digest.Size); err != nil {
		return err
	}
	if digest.Size > c.UploadMetaInfo.SizeLimit {
		return ErrUploadTooLarge
	}
	// 闪传
	if fastInfo, err = c.RapidUpload(
		digest.Size, fileName, dirID, digest.PreID, digest.QuickID, r,
	); err != nil {
		return err
	}
	if ok, err := fastInfo.Ok(); err != nil {
		return err
	} else if ok {
		return nil
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// 闪传失败，普通上传
	return c.UploadByOSS(&fastInfo.UploadOSSParams, r, dirID)
}

// getOSSEndpoint get oss endpoint 利用阿里云内网上传文件，需要在阿里云服务器上运行本程序，同时也需要115在服务器的所在地域开通了阿里云OSS
func (c *Pan115Client) getOSSEndpoint(enableInternalUpload bool) string {
	if enableInternalUpload {
		uploadEndpoint := UploadEndpointResp{}
		if err := c.GetUploadEndpoint(&uploadEndpoint); err != nil {
			// Do not log the raw error: transport/API errors may include request
			// context. The error type is enough to explain why internal OSS was
			// disabled without exposing URLs, headers, cookies, or credentials.
			c.debugf("internal upload endpoint discovery failed; falling back to public endpoint error_type=%T", err)
			return OSSEndpoint
		}
		i := strings.Index(uploadEndpoint.Endpoint, ".aliyuncs.com")
		if i > -1 {
			endpoint := uploadEndpoint.Endpoint[:i] + "-internal" + uploadEndpoint.Endpoint[i:]
			return endpoint
		}
		c.debugf("internal upload endpoint is not Aliyun-compatible; falling back to public endpoint")
	}
	return OSSEndpoint
}

// GetOSSEndpoint get oss endpoint 利用阿里云内网上传文件，需要在阿里云服务器上运行本程序，同时也需要115在服务器的所在地域开通了阿里云OSS
func (c *Pan115Client) GetOSSEndpoint(enableInternalUpload bool) string {
	return c.getOSSEndpoint(enableInternalUpload)
}

func validateUploadOSSParams(params *UploadOSSParams) error {
	if params == nil {
		return fmt.Errorf("OSS upload params are nil: %w", ErrWrongParams)
	}
	if strings.TrimSpace(params.Bucket) == "" || strings.TrimSpace(params.Object) == "" || strings.TrimSpace(params.SHA1) == "" {
		return fmt.Errorf("OSS upload params are incomplete: %w", ErrWrongParams)
	}
	return nil
}

// UploadByOSS use aliyun sdk to upload
func (c *Pan115Client) UploadByOSS(params *UploadOSSParams, r io.Reader, dirID string) error {
	if err := validateUploadOSSParams(params); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("OSS upload reader is nil: %w", ErrWrongParams)
	}
	ossToken, err := c.GetOSSToken()
	if err != nil {
		return err
	}
	ossClient, err := oss.New(c.getOSSEndpoint(c.UseInternalUpload), ossToken.AccessKeyID, ossToken.AccessKeySecret)
	if err != nil {
		return err
	}
	bucket, err := ossClient.Bucket(params.Bucket)
	if err != nil {
		return err
	}

	if err = bucket.PutObject(params.Object, r, OssOption(params, ossToken)...); err != nil {
		return err
	}

	return c.checkUploadStatus(dirID, params.SHA1)
}

// CheckUploadStatus verifies that an uploaded object with the expected SHA1 is
// visible in the target directory. It is the exported counterpart of the
// legacy ordinary OSS upload verification step.
func (c *Pan115Client) CheckUploadStatus(dirID, sha1 string) error {
	return c.checkUploadStatus(dirID, sha1)
}

func (c *Pan115Client) checkUploadStatus(dirID, sha1 string) error {
	// 验证上传是否成功
	req := c.NewRequest().ForceContentType("application/json;charset=UTF-8")
	opts := []GetFileOptions{
		WithOrder(FileOrderByTime),
		WithShowDirEnable(false),
		WithAsc(false),
		WithLimit(500),
	}
	fResp, err := GetFiles(req, dirID, opts...)
	if err != nil {
		return err
	}
	for _, fileInfo := range fResp.Files {
		if fileInfo.Sha1 == sha1 {
			return nil
		}
	}
	return ErrUploadFailed
}

// GetOSSToken get oss token for oss upload
func (c *Pan115Client) GetOSSToken() (*UploadOSSTokenResp, error) {
	result := UploadOSSTokenResp{}
	req := c.NewRequest().
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&result)

	resp, err := req.Get(ApiUploadOSSToken)
	if err = CheckErr(err, &result, resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.AccessKeyID) == "" || strings.TrimSpace(result.AccessKeySecret) == "" || strings.TrimSpace(result.SecurityToken) == "" {
		return nil, fmt.Errorf("OSS upload token is incomplete: %w", ErrUnexpected)
	}
	return &result, nil
}

// UploadSHA1 upload a sha1, alias of RapidUpload
// Deprecated: As of v1.0.22, this function simply calls [RapidUpload].
func (c *Pan115Client) UploadSHA1(fileSize int64, fileName, dirID, preID, fileID string, r io.ReadSeeker) (*UploadInitResp, error) {
	return c.RapidUpload(fileSize, fileName, dirID, preID, fileID, r)
}

// RapidUpload rapid upload
func (c *Pan115Client) RapidUpload(fileSize int64, fileName, dirID, preID, fileID string, r io.ReadSeeker) (*UploadInitResp, error) {
	if r == nil {
		return nil, fmt.Errorf("rapid upload reader is nil: %w", ErrWrongParams)
	}
	if fileSize < 0 {
		return nil, fmt.Errorf("rapid upload file size is negative: %w", ErrWrongParams)
	}
	var (
		ecdhCipher   *cipher.EcdhCipher
		encrypted    []byte
		decrypted    []byte
		encodedToken string
		err          error
		target       = "U_1_" + dirID
		bodyBytes    []byte
		result       = UploadInitResp{}
		fileSizeStr  = strconv.FormatInt(fileSize, 10)
	)
	if ecdhCipher, err = cipher.NewEcdhCipher(); err != nil {
		return nil, err
	}

	if ok, err := c.UploadAvailable(); !ok || err != nil {
		return nil, err
	}

	userID := strconv.FormatInt(c.UserID, 10)
	form := url.Values{}
	form.Set("appid", "0")
	form.Set("appversion", appVer)
	form.Set("userid", userID)
	form.Set("filename", fileName)
	form.Set("filesize", fileSizeStr)
	form.Set("fileid", fileID)
	form.Set("target", target)
	form.Set("sig", c.GenerateSignature(fileID, target))
	form.Set("topupload", "true")

	signKey, signVal := "", ""
	challengeCount := 0
	for {
		result = UploadInitResp{}
		t := NowMilli()

		if encodedToken, err = ecdhCipher.EncodeToken(t.ToInt64()); err != nil {
			return nil, err
		}

		params := map[string]string{
			"k_ec": encodedToken,
		}

		form.Set("t", t.String())
		form.Set("token", c.GenerateToken(fileID, preID, t.String(), fileSizeStr, signKey, signVal))
		if signKey != "" && signVal != "" {
			form.Set("sign_key", signKey)
			form.Set("sign_val", signVal)
		}
		if encrypted, err = ecdhCipher.Encrypt([]byte(form.Encode())); err != nil {
			return nil, err
		}

		req := c.NewRequest().
			SetQueryParams(params).
			SetBody(encrypted).
			SetHeaderVerbatim("Content-Type", "application/x-www-form-urlencoded").
			SetDoNotParseResponse(true)
		resp, err := req.Post(ApiUploadInit)
		if err != nil {
			return nil, sanitizeHTTPError(err)
		}
		if resp == nil {
			return nil, fmt.Errorf("rapid upload returned no response: %w", ErrUnexpected)
		}
		if err := validateRestyHTTPStatus(resp); err != nil {
			if data := resp.RawBody(); data != nil {
				_ = data.Close()
			}
			return nil, err
		}
		data := resp.RawBody()
		if data == nil {
			return nil, fmt.Errorf("rapid upload returned no response body: %w", ErrUnexpected)
		}
		bodyBytes, err = io.ReadAll(data)
		_ = data.Close()
		if err != nil {
			return nil, err
		}
		if decrypted, err = ecdhCipher.Decrypt(bodyBytes); err != nil {
			return nil, err
		}
		if err = CheckErr(json.Unmarshal(decrypted, &result), &result, resp); err != nil {
			return nil, err
		}
		result.SHA1 = fileID
		var retry bool
		signKey, signVal, retry, err = c.resolveRapidUploadSignChallenge(&result, challengeCount, r)
		if err != nil {
			return nil, err
		}
		if !retry {
			break
		}
		challengeCount++
	}

	return &result, nil
}

const (
	md5Salt                      = "Qclm8MGWUv59TnrR0XPg"
	appVer                       = "27.0.5.7"
	maxRapidUploadSignChallenges = 3
)

func (c *Pan115Client) resolveRapidUploadSignChallenge(result *UploadInitResp, challengeCount int, r io.ReadSeeker) (signKey, signVal string, retry bool, err error) {
	if result == nil {
		return "", "", false, fmt.Errorf("rapid upload challenge response is nil: %w", ErrUnexpected)
	}
	if result.Status != 7 {
		return "", "", false, nil
	}
	if challengeCount >= maxRapidUploadSignChallenges {
		return "", "", false, fmt.Errorf("rapid upload exceeded %d sign challenges: %w", maxRapidUploadSignChallenges, ErrUnexpected)
	}
	signKey = strings.TrimSpace(result.SignKey)
	signCheck := strings.TrimSpace(result.SignCheck)
	if signKey == "" || signCheck == "" {
		return "", "", false, fmt.Errorf("rapid upload sign challenge is incomplete: %w", ErrUnexpected)
	}
	signVal, err = c.UploadDigestRange(r, signCheck)
	if err != nil {
		return "", "", false, fmt.Errorf("rapid upload sign challenge failed: %w", err)
	}
	return signKey, signVal, true, nil
}

func (c *Pan115Client) UploadDigestRange(r io.ReadSeeker, rangeSpec string) (result string, err error) {
	if r == nil {
		return "", fmt.Errorf("upload digest reader is nil: %w", ErrWrongParams)
	}
	rangeSpec = strings.TrimSpace(rangeSpec)
	startRaw, endRaw, ok := strings.Cut(rangeSpec, "-")
	if !ok || startRaw == "" || endRaw == "" || strings.Contains(endRaw, "-") {
		return "", fmt.Errorf("invalid upload digest range %q: %w", rangeSpec, ErrWrongParams)
	}
	start, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil || start < 0 {
		return "", fmt.Errorf("invalid upload digest range %q: %w", rangeSpec, ErrWrongParams)
	}
	end, err := strconv.ParseInt(endRaw, 10, 64)
	if err != nil || end < start {
		return "", fmt.Errorf("invalid upload digest range %q: %w", rangeSpec, ErrWrongParams)
	}
	h := sha1.New()
	if _, err = r.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	if _, err = io.CopyN(h, r, end-start+1); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

func (c *Pan115Client) GenerateSignature(fileID, target string) string {
	sh1hash := sha1.Sum([]byte(strconv.FormatInt(c.UserID, 10) + fileID + target + "0"))
	sigStr := c.Userkey + hex.EncodeToString(sh1hash[:]) + "000000"
	sh1Sig := sha1.Sum([]byte(sigStr))
	return strings.ToUpper(hex.EncodeToString(sh1Sig[:]))
}

func (c *Pan115Client) GenerateToken(fileID, preID, timeStamp, fileSize, signKey, signVal string) string {
	userID := strconv.FormatInt(c.UserID, 10)
	userIDMd5 := md5.Sum([]byte(userID))
	tokenMd5 := md5.Sum([]byte(md5Salt + fileID + fileSize + signKey + signVal + userID + timeStamp + hex.EncodeToString(userIDMd5[:]) + appVer))
	return hex.EncodeToString(tokenMd5[:])
}

// UploadFastOrByMultipart upload by mutipart blocks when unable to rapid upload
// Deprecated: As of v1.0.22, this function simply calls [RapidUploadOrByMultipart].
func (c *Pan115Client) UploadFastOrByMultipart(dirID, fileName string, fileSize int64, r *os.File, opts ...UploadMultipartOption) error {
	return c.RapidUploadOrByMultipart(dirID, fileName, fileSize, r, opts...)
}

// RapidUploadOrByMultipart upload by mutipart blocks when unable to rapid upload
func (c *Pan115Client) RapidUploadOrByMultipart(dirID, fileName string, fileSize int64, r *os.File, opts ...UploadMultipartOption) error {
	if r == nil {
		return fmt.Errorf("upload file is nil: %w", ErrWrongParams)
	}
	if fileSize < 0 {
		return fmt.Errorf("upload file size is negative: %w", ErrWrongParams)
	}
	var (
		err      error
		digest   *hash.DigestResult
		fastInfo *UploadInitResp
	)

	if ok, err := c.UploadAvailable(); err != nil || !ok {
		return err
	}
	if fileSize > c.UploadMetaInfo.SizeLimit {
		return ErrUploadTooLarge
	}
	if digest, err = c.GetDigestResult(r); err != nil {
		return err
	}
	if err := validateUploadSourceSize(fileSize, digest.Size); err != nil {
		return err
	}
	if digest.Size > c.UploadMetaInfo.SizeLimit {
		return ErrUploadTooLarge
	}
	// 闪传
	if fastInfo, err = c.RapidUpload(
		digest.Size, fileName, dirID, digest.PreID, digest.QuickID, r,
	); err != nil {
		return err
	}
	if ok, err := fastInfo.Ok(); err != nil {
		return err
	} else if ok {
		return nil
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 闪传失败，上传
	if digest.Size <= KB { // 文件大小小于1KB，改用普通模式上传
		return c.UploadByOSS(&fastInfo.UploadOSSParams, r, dirID)
	}
	// 分片上传
	return c.UploadByMultipart(&fastInfo.UploadOSSParams, digest.Size, r, dirID, opts...)
}

func validateUploadCompletionMetadata(result *UploadResult, expectedSHA1 string, expectedSize int64) (needsVisibilityCheck bool, err error) {
	if result == nil {
		return false, fmt.Errorf("upload completion result is nil: %w", ErrUnexpected)
	}
	expectedSHA1 = strings.TrimSpace(expectedSHA1)
	if expectedSHA1 == "" || expectedSize < 0 {
		return false, fmt.Errorf("upload completion expectation is invalid: %w", ErrWrongParams)
	}
	responseSHA1 := strings.TrimSpace(result.Data.Sha1)
	fileID := strings.TrimSpace(result.Data.FileID)
	if responseSHA1 != "" && !strings.EqualFold(responseSHA1, expectedSHA1) {
		return false, fmt.Errorf("upload completion SHA1 mismatch: expected=%q response=%q: %w", expectedSHA1, responseSHA1, ErrUnexpected)
	}
	if result.Data.FileSize < 0 {
		return false, fmt.Errorf("upload completion returned negative file size: %w", ErrUnexpected)
	}
	if result.Data.FileSize > 0 && int64(result.Data.FileSize) != expectedSize {
		return false, fmt.Errorf("upload completion size mismatch: expected=%d response=%d: %w", expectedSize, result.Data.FileSize, ErrUnexpected)
	}
	if fileID == "" || responseSHA1 == "" || (expectedSize > 0 && result.Data.FileSize == 0) {
		return true, nil
	}
	return false, nil
}

// UploadByMultipart uploads multipart blocks sequentially. OSS sequential mode
// requires ordered parts, so a synchronous loop avoids worker/channel leaks and
// shared token/error races while preserving the existing retry semantics.
func (c *Pan115Client) UploadByMultipart(params *UploadOSSParams, fileSize int64, f *os.File, dirID string, opts ...UploadMultipartOption) error {
	if err := validateUploadOSSParams(params); err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("multipart upload file is nil: %w", ErrWrongParams)
	}
	if fileSize < 0 {
		return fmt.Errorf("multipart upload file size is negative: %w", ErrWrongParams)
	}
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat multipart upload file: %w", err)
	}
	if stat.Size() != fileSize {
		return fmt.Errorf("multipart upload file size mismatch: declared=%d actual=%d: %w", fileSize, stat.Size(), ErrWrongParams)
	}

	options := DefaultUploadMultipartOptions()
	for _, apply := range opts {
		if apply != nil {
			apply(options)
		}
	}
	if options.Timeout <= 0 {
		return fmt.Errorf("multipart upload timeout must be positive: %w", ErrWrongParams)
	}
	if options.TokenRefreshTime <= 0 {
		return fmt.Errorf("multipart upload token refresh interval must be positive: %w", ErrWrongParams)
	}
	options.ThreadsNum = 1

	ossToken, err := c.GetOSSToken()
	if err != nil {
		return err
	}
	endpoint := c.getOSSEndpoint(c.UseInternalUpload)
	openBucket := func(token *UploadOSSTokenResp) (*oss.Bucket, error) {
		ossClient, err := oss.New(
			endpoint,
			token.AccessKeyID,
			token.AccessKeySecret,
			oss.EnableMD5(true),
			oss.EnableCRC(true),
		)
		if err != nil {
			return nil, err
		}
		return ossClient.Bucket(params.Bucket)
	}
	bucket, err := openBucket(ossToken)
	if err != nil {
		return err
	}

	chunks, err := SplitFile(f.Name(), fileSize)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return fmt.Errorf("multipart split returned no chunks: %w", ErrUnexpected)
	}

	imur, err := bucket.InitiateMultipartUpload(params.Object,
		oss.SetHeader(OssSecurityTokenHeaderName, ossToken.SecurityToken),
		oss.UserAgentHeader(OSSUserAgent),
		oss.EnableSha1(),
		oss.Sequential(),
	)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(options.TokenRefreshTime)
	defer ticker.Stop()
	timeout := time.NewTimer(options.Timeout)
	defer timeout.Stop()

	parts := make([]oss.UploadPart, 0, len(chunks))
	for _, chunk := range chunks {
		select {
		case <-timeout.C:
			return fmt.Errorf("multipart upload timed out after %s: %w", options.Timeout, ErrUnexpected)
		default:
		}

		buf := make([]byte, chunk.Size)
		if _, err := f.ReadAt(buf, chunk.Offset); err != nil {
			return fmt.Errorf("read multipart upload part %d: %w", chunk.Number, err)
		}

		var part oss.UploadPart
		var partErr error
		for retry := 0; retry < 3; retry++ {
			select {
			case <-timeout.C:
				return fmt.Errorf("multipart upload timed out after %s: %w", options.Timeout, ErrUnexpected)
			default:
			}
			select {
			case <-ticker.C:
				refreshed, refreshErr := c.GetOSSToken()
				if refreshErr != nil {
					return errors.Wrap(refreshErr, "refresh OSS upload token")
				}
				refreshedBucket, bucketErr := openBucket(refreshed)
				if bucketErr != nil {
					return errors.Wrap(bucketErr, "recreate OSS bucket after token refresh")
				}
				ossToken = refreshed
				bucket = refreshedBucket
			default:
			}

			part, partErr = bucket.UploadPart(
				imur,
				bytes.NewReader(buf),
				chunk.Size,
				chunk.Number,
				OssOption(params, ossToken)...,
			)
			if partErr == nil {
				break
			}
		}
		if partErr != nil {
			return errors.Wrapf(partErr, "upload %s part %d after 3 attempts", f.Name(), chunk.Number)
		}
		parts = append(parts, part)
	}

	select {
	case <-timeout.C:
		return fmt.Errorf("multipart upload timed out after %s: %w", options.Timeout, ErrUnexpected)
	default:
	}

	var bodyBytes []byte
	if _, err := bucket.CompleteMultipartUpload(imur, parts,
		append(
			OssOption(params, ossToken),
			oss.CallbackResult(&bodyBytes),
		)...); err != nil {
		return err
	}

	var uploadResult UploadResult
	if err = json.Unmarshal(bodyBytes, &uploadResult); err != nil {
		return err
	}
	if err = uploadResult.Err(string(bodyBytes)); err != nil {
		return err
	}
	needsVisibilityCheck, err := validateUploadCompletionMetadata(&uploadResult, params.SHA1, fileSize)
	if err != nil {
		return err
	}
	if needsVisibilityCheck {
		return c.checkUploadStatus(dirID, params.SHA1)
	}
	return nil
}

func splitFilePartNum(fileSize int64) int {
	for i := int64(1); i < 10; i++ {
		if fileSize < i*GB {
			return int(i * 1000)
		}
	}
	return 10000
}

// SplitFile splits a file into OSS multipart chunks.
func SplitFile(filePath string, fileSize int64) (chunks []oss.FileChunk, err error) {
	if fileSize < 0 {
		return nil, fmt.Errorf("split file size is negative: %w", ErrWrongParams)
	}
	chunks, err = oss.SplitFileByPartNum(filePath, splitFilePartNum(fileSize))
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("split file returned no chunks: %w", ErrUnexpected)
	}
	if chunks[0].Size < 100*KB {
		chunks, err = oss.SplitFileByPartSize(filePath, 100*KB)
		if err != nil {
			return nil, err
		}
		if len(chunks) == 0 {
			return nil, fmt.Errorf("split file by part size returned no chunks: %w", ErrUnexpected)
		}
	}
	return chunks, nil
}

// OssOption get options
func OssOption(params *UploadOSSParams, ossToken *UploadOSSTokenResp) []oss.Option {
	if params == nil || ossToken == nil {
		return nil
	}
	options := []oss.Option{
		oss.SetHeader(OssSecurityTokenHeaderName, ossToken.SecurityToken),
		oss.Callback(base64.StdEncoding.EncodeToString([]byte(params.Callback.Callback))),
		oss.CallbackVar(base64.StdEncoding.EncodeToString([]byte(params.Callback.CallbackVar))),
		oss.UserAgentHeader(OSSUserAgent),
	}
	return options
}
