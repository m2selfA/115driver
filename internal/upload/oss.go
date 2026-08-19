package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

const ossTokenRefreshMargin = 5 * time.Minute

type ossBucketHandle struct {
	bucket    *oss.Bucket
	transport http.RoundTripper
}

type ossBucketSnapshot struct {
	handle     *ossBucketHandle
	token      driver.UploadOSSTokenResp
	generation uint64
}

type ossTransportFactory func(transfer.NetworkPath) (http.RoundTripper, error)
type ossTokenFetcher func() (*driver.UploadOSSTokenResp, error)

type ossBucketPool struct {
	mu               sync.Mutex
	endpoint         string
	bucketName       string
	token            *driver.UploadOSSTokenResp
	generation       uint64
	refreshed        time.Time
	handles          map[int]*ossBucketHandle
	transportFactory ossTransportFactory
	tokenFetcher     ossTokenFetcher
}

func newOSSBucketPool(client *driver.Pan115Client, endpoint, bucketName string) *ossBucketPool {
	pool := &ossBucketPool{
		endpoint: endpoint, bucketName: bucketName, handles: make(map[int]*ossBucketHandle),
		transportFactory: func(path transfer.NetworkPath) (http.RoundTripper, error) { return transfer.NewTransport(path) },
	}
	if client != nil {
		pool.tokenFetcher = client.GetOSSToken
	}
	return pool
}

func (pool *ossBucketPool) close() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.closeHandlesLocked()
}

func (pool *ossBucketPool) snapshot(path transfer.NetworkPath) (ossBucketSnapshot, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if err := pool.ensureFreshTokenLocked(false); err != nil {
		return ossBucketSnapshot{}, err
	}
	return pool.snapshotLocked(path)
}

func (pool *ossBucketPool) refreshIfStale(path transfer.NetworkPath, staleGeneration uint64) (ossBucketSnapshot, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.generation == staleGeneration {
		if err := pool.ensureFreshTokenLocked(true); err != nil {
			return ossBucketSnapshot{}, err
		}
	}
	return pool.snapshotLocked(path)
}

func (pool *ossBucketPool) snapshotLocked(path transfer.NetworkPath) (ossBucketSnapshot, error) {
	handle := pool.handles[path.InterfaceIndex]
	if handle == nil {
		transport, err := pool.transportFactory(path)
		if err != nil {
			return ossBucketSnapshot{}, fmt.Errorf("create bound OSS transport for %s: %w", path, err)
		}
		httpClient := &http.Client{Transport: transport}
		ossClient, err := oss.New(
			pool.endpoint,
			pool.token.AccessKeyID,
			pool.token.AccessKeySecret,
			oss.HTTPClient(httpClient),
			oss.SecurityToken(pool.token.SecurityToken),
			oss.EnableMD5(true),
			oss.EnableCRC(true),
			oss.UserAgent(driver.OSSUserAgent),
		)
		if err != nil {
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
			return ossBucketSnapshot{}, fmt.Errorf("create OSS client for %s: %w", path, err)
		}
		bucket, err := ossClient.Bucket(pool.bucketName)
		if err != nil {
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
			return ossBucketSnapshot{}, fmt.Errorf("open OSS bucket for %s: %w", path, err)
		}
		handle = &ossBucketHandle{bucket: bucket, transport: transport}
		pool.handles[path.InterfaceIndex] = handle
	}
	return ossBucketSnapshot{handle: handle, token: *pool.token, generation: pool.generation}, nil
}

func (pool *ossBucketPool) ensureFreshTokenLocked(force bool) error {
	now := time.Now()
	if !force && pool.token != nil {
		if !pool.token.Expiration.IsZero() && now.Add(ossTokenRefreshMargin).Before(pool.token.Expiration) {
			return nil
		}
		if pool.token.Expiration.IsZero() && now.Sub(pool.refreshed) < 50*time.Minute {
			return nil
		}
	}
	if pool.tokenFetcher == nil {
		return errors.New("OSS token provider is unavailable")
	}
	token, err := pool.tokenFetcher()
	if err != nil {
		return fmt.Errorf("get OSS upload token: %w", err)
	}
	if token == nil || strings.TrimSpace(token.AccessKeyID) == "" || strings.TrimSpace(token.AccessKeySecret) == "" {
		return errors.New("OSS upload token is incomplete")
	}
	pool.closeHandlesLocked()
	pool.token = token
	pool.generation++
	pool.refreshed = now
	return nil
}

func (pool *ossBucketPool) closeHandlesLocked() {
	for index, handle := range pool.handles {
		if handle != nil {
			if closer, ok := handle.transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
		delete(pool.handles, index)
	}
}

func isOSSAuthError(err error) bool {
	var serviceError oss.ServiceError
	if errors.As(err, &serviceError) {
		return serviceError.StatusCode == http.StatusUnauthorized || serviceError.StatusCode == http.StatusForbidden
	}
	var serviceErrorPtr *oss.ServiceError
	return errors.As(err, &serviceErrorPtr) && serviceErrorPtr != nil &&
		(serviceErrorPtr.StatusCode == http.StatusUnauthorized || serviceErrorPtr.StatusCode == http.StatusForbidden)
}

func ossRequestOptions(ctx context.Context, token driver.UploadOSSTokenResp) []oss.Option {
	return []oss.Option{
		oss.WithContext(ctx),
		oss.SetHeader(driver.OssSecurityTokenHeaderName, token.SecurityToken),
		oss.UserAgentHeader(driver.OSSUserAgent),
	}
}

func uploadPartWithRefresh(ctx context.Context, pool *ossBucketPool, imur oss.InitiateMultipartUploadResult, file *os.File, path transfer.NetworkPath, job transfer.UploadPartJob) (transfer.UploadPartResult, error) {
	upload := func(snapshot ossBucketSnapshot) (transfer.UploadPartResult, error) {
		reader := io.NewSectionReader(file, job.Offset, job.Size)
		part, err := snapshot.handle.bucket.UploadPart(imur, reader, job.Size, job.PartNumber, ossRequestOptions(ctx, snapshot.token)...)
		if err != nil {
			return transfer.UploadPartResult{}, err
		}
		return transfer.UploadPartResult{PartNumber: part.PartNumber, ETag: part.ETag, BytesUploaded: job.Size}, nil
	}

	snapshot, err := pool.snapshot(path)
	if err != nil {
		return transfer.UploadPartResult{}, err
	}
	result, err := upload(snapshot)
	if !isOSSAuthError(err) {
		return result, err
	}
	refreshed, refreshErr := pool.refreshIfStale(path, snapshot.generation)
	if refreshErr != nil {
		return transfer.UploadPartResult{}, errors.Join(err, refreshErr)
	}
	return upload(refreshed)
}

func initiateMultipart(ctx context.Context, pool *ossBucketPool, paths []transfer.NetworkPath, object string, sequential bool) (oss.InitiateMultipartUploadResult, error) {
	var errs []error
	for _, path := range paths {
		snapshot, err := pool.snapshot(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		initiate := func(current ossBucketSnapshot) (oss.InitiateMultipartUploadResult, error) {
			opts := ossRequestOptions(ctx, current.token)
			if sequential {
				opts = append(opts, oss.EnableSha1(), oss.Sequential())
			}
			return current.handle.bucket.InitiateMultipartUpload(object, opts...)
		}
		imur, err := initiate(snapshot)
		if isOSSAuthError(err) {
			refreshed, refreshErr := pool.refreshIfStale(path, snapshot.generation)
			if refreshErr != nil {
				err = errors.Join(err, refreshErr)
			} else {
				imur, err = initiate(refreshed)
			}
		}
		if err == nil {
			return imur, nil
		}
		errs = append(errs, fmt.Errorf("initiate multipart through %s: %w", path, err))
	}
	return oss.InitiateMultipartUploadResult{}, errors.Join(errs...)
}

func multipartCallbackParams(params *driver.UploadOSSParams, sequential bool) driver.UploadOSSParams {
	if params == nil {
		return driver.UploadOSSParams{}
	}
	clone := *params
	if !sequential {
		clone.Callback.Callback = strings.ReplaceAll(clone.Callback.Callback, "${sha1}", clone.SHA1)
	}
	return clone
}

func completeMultipart(ctx context.Context, pool *ossBucketPool, paths []transfer.NetworkPath, imur oss.InitiateMultipartUploadResult, parts []oss.UploadPart, params *driver.UploadOSSParams, sequential bool) ([]byte, error) {
	callbackParams := multipartCallbackParams(params, sequential)
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	var errs []error
	for _, path := range paths {
		snapshot, err := pool.snapshot(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		complete := func(current ossBucketSnapshot) ([]byte, error) {
			var body []byte
			opts := append(driver.OssOption(&callbackParams, &current.token), oss.WithContext(ctx), oss.CallbackResult(&body))
			_, err := current.handle.bucket.CompleteMultipartUpload(imur, parts, opts...)
			return body, err
		}
		body, err := complete(snapshot)
		if isOSSAuthError(err) {
			refreshed, refreshErr := pool.refreshIfStale(path, snapshot.generation)
			if refreshErr != nil {
				err = errors.Join(err, refreshErr)
			} else {
				body, err = complete(refreshed)
			}
		}
		if err == nil {
			return body, nil
		}
		errs = append(errs, fmt.Errorf("complete multipart through %s: %w", path, err))
	}
	return nil, errors.Join(errs...)
}

func abortMultipart(pool *ossBucketPool, paths []transfer.NetworkPath, imur oss.InitiateMultipartUploadResult) error {
	abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for _, path := range paths {
		snapshot, err := pool.snapshot(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = snapshot.handle.bucket.AbortMultipartUpload(imur, ossRequestOptions(abortCtx, snapshot.token)...)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func parseUploadCallback(body []byte, expectedSHA1 string) error {
	if len(body) == 0 {
		return nil
	}
	var result driver.UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode 115 upload callback: %w", err)
	}
	if err := result.Err(string(body)); err != nil {
		return err
	}
	if result.Data.Sha1 != "" && expectedSHA1 != "" && !strings.EqualFold(result.Data.Sha1, expectedSHA1) {
		return fmt.Errorf("115 upload callback SHA1 mismatch: got %s", result.Data.Sha1)
	}
	return nil
}

func putObjectBound(ctx context.Context, pool *ossBucketPool, path transfer.NetworkPath, file *os.File, size int64, params *driver.UploadOSSParams) error {
	put := func(snapshot ossBucketSnapshot) error {
		var body []byte
		reader := io.NewSectionReader(file, 0, size)
		opts := append(driver.OssOption(params, &snapshot.token), oss.WithContext(ctx), oss.CallbackResult(&body))
		if err := snapshot.handle.bucket.PutObject(params.Object, reader, opts...); err != nil {
			return err
		}
		return parseUploadCallback(body, params.SHA1)
	}
	snapshot, err := pool.snapshot(path)
	if err != nil {
		return err
	}
	if err = put(snapshot); !isOSSAuthError(err) {
		return err
	}
	refreshed, refreshErr := pool.refreshIfStale(path, snapshot.generation)
	if refreshErr != nil {
		return errors.Join(err, refreshErr)
	}
	return put(refreshed)
}

func buildUploadPartJobs(fileSize, requestedChunkSize int64) ([]transfer.UploadPartJob, int64, error) {
	if fileSize < 0 {
		return nil, 0, errors.New("upload file size must be >= 0")
	}
	if fileSize == 0 {
		return nil, requestedChunkSize, nil
	}
	chunkSize := requestedChunkSize
	if chunkSize < MinPartSize {
		chunkSize = MinPartSize
	}
	minimumForPartLimit := fileSize / MaxPartCount
	if fileSize%MaxPartCount != 0 {
		minimumForPartLimit++
	}
	if minimumForPartLimit > chunkSize {
		chunkSize = minimumForPartLimit
	}
	jobs := make([]transfer.UploadPartJob, 0, int(fileSize/chunkSize)+1)
	for offset, partNumber := int64(0), 1; offset < fileSize; partNumber++ {
		size := chunkSize
		if remaining := fileSize - offset; size > remaining {
			size = remaining
		}
		jobs = append(jobs, transfer.UploadPartJob{PartNumber: partNumber, Offset: offset, Size: size})
		offset += size
	}
	if len(jobs) > MaxPartCount {
		return nil, 0, fmt.Errorf("upload would require %d parts, maximum is %d", len(jobs), MaxPartCount)
	}
	return jobs, chunkSize, nil
}
