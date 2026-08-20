package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type Result struct {
	Rapid         bool
	Multipart     bool
	Sequential    bool
	Resumed       bool
	Verified      bool
	Skipped       bool
	ResumedParts  int
	BytesUploaded int64
	PartCount     int
	ChunkSize     int64
	NetworkPaths  []transfer.NetworkPath
	Duration      time.Duration
	SHA1          string
}

// uploadFileWithoutResume performs 115 rapid-upload negotiation first. When
// object data is required, the OSS data plane is bound to selected local network
// interfaces. Protocol callbacks that require ${sha1} use one interface with
// OSS sequential SHA1 context; only callback forms that do not require that
// context may use ordinary multi-interface multipart upload.
func uploadFileWithoutResume(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options) (Result, error) {
	started := time.Now()
	result := Result{}
	if client == nil {
		return result, errors.New("115 upload client is nil")
	}
	if file == nil {
		return result, errors.New("upload file is nil")
	}
	options.Interfaces = strings.TrimSpace(options.Interfaces)
	if err := options.validate(); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if ok, err := client.UploadAvailable(); err != nil || !ok {
		return result, err
	}
	if fileSize < 0 {
		return result, errors.New("upload file size must be >= 0")
	}
	if client.UploadMetaInfo != nil && fileSize > client.UploadMetaInfo.SizeLimit {
		return result, driver.ErrUploadTooLarge
	}
	digest, err := resolveUploadDigest(file, fileSize, options.PreparedDigest)
	if err != nil {
		return result, err
	}
	result.SHA1 = digest.SHA1
	if options.Progress != nil {
		options.Progress("Checking 115 rapid upload...")
	}
	fastInfo, err := client.RapidUpload(digest.Size, fileName, dirID, digest.PreID, digest.SHA1, file)
	if err != nil {
		return result, err
	}
	if fastInfo == nil {
		return result, errors.New("115 rapid upload returned no initialization result")
	}
	rapid, err := fastInfo.Ok()
	if err != nil {
		return result, err
	}
	if rapid {
		result.Rapid = true
		result.BytesUploaded = fileSize
		options.reportBytes(fileSize, fileSize)
		result.Duration = time.Since(started)
		return result, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek upload file for OSS transfer: %w", err)
	}
	params := fastInfo.UploadOSSParams
	if params.SHA1 == "" {
		params.SHA1 = digest.SHA1
	}
	if !strings.EqualFold(params.SHA1, digest.SHA1) {
		return result, errors.New("115 upload initialization returned a different file SHA1")
	}
	requireSequentialUploadCompatibility(&options, &params)
	endpoint := client.GetOSSEndpoint(client.UseInternalUpload)
	probeURL, err := buildOSSProbeURL(endpoint, params.Bucket)
	if err != nil {
		return result, err
	}
	if options.HealthTracker == nil {
		options.HealthTracker = transfer.NewDefaultNetworkHealthTracker()
	}
	selection, err := resolveUploadPaths(ctx, options.Interfaces, probeURL)
	if err != nil {
		return result, err
	}
	if options.Compatibility != nil {
		options.Compatibility.ObserveNetworkPaths(selection.Paths)
	}
	selection = applyUploadCompatibilitySelection(options, selection)
	result.NetworkPaths = append([]transfer.NetworkPath(nil), selection.Paths...)
	if options.Progress != nil {
		if selection.Warning != nil {
			options.Progress(fmt.Sprintf("Network warning: %v", selection.Warning))
		}
		if options.forceSequential {
			options.Progress(fmt.Sprintf("Using %d interface(s) for sequential compatibility failover (one ordered part at a time)...", len(selection.Paths)))
		} else {
			options.Progress(fmt.Sprintf("Using %d interface(s), up to %d connection(s) each for OSS upload...", len(selection.Paths), options.WorkersPerInterface))
		}
	}
	pool := newOSSBucketPool(client, endpoint, params.Bucket)
	defer pool.close()

	// Preserve the old ordinary upload mode for tiny files while still binding
	// its one data stream to the selected NIC.
	if fileSize <= 1024 {
		if err := putObjectBound(ctx, pool, selection.Paths[0], file, fileSize, &params); err != nil {
			return result, err
		}
		if err := client.CheckUploadStatus(dirID, params.SHA1); err != nil {
			return result, err
		}
		options.reportBytes(fileSize, fileSize)
		result.BytesUploaded = fileSize
		result.Duration = time.Since(started)
		return result, nil
	}

	jobs, effectiveChunkSize, err := buildUploadPartJobs(fileSize, options.ChunkSize)
	if err != nil {
		return result, err
	}
	sequential := options.forceSequential
	result.Multipart = true
	result.Sequential = sequential
	result.PartCount = len(jobs)
	result.ChunkSize = effectiveChunkSize

	imur, err := initiateMultipart(ctx, pool, selection.Paths, params.Object, sequential)
	if err != nil {
		return result, err
	}
	completed := false
	defer func() {
		if !completed {
			_ = abortMultipart(pool, selection.Paths, imur)
		}
	}()

	var completedBytes atomic.Int64
	report, scheduleErr := transfer.ScheduleUploadParts(ctx, selection.Paths, jobs, func(ctx context.Context, path transfer.NetworkPath, job transfer.UploadPartJob) (transfer.UploadPartResult, error) {
		partResult, partErr := uploadPartWithRefresh(ctx, pool, imur, file, path, job)
		if partErr == nil {
			options.reportBytes(completedBytes.Add(job.Size), fileSize)
		}
		return partResult, partErr
	}, transfer.WithUploadPartRetries(options.Retries), transfer.WithUploadPartWorkersPerInterface(options.WorkersPerInterface), transfer.WithUploadPartHealthTracker(options.HealthTracker), transfer.WithUploadPartPreserveOrder(sequential))
	if scheduleErr != nil {
		return result, scheduleErr
	}
	parts := make([]oss.UploadPart, 0, len(report.Results))
	for _, partResult := range report.Results {
		if partResult.Err != nil {
			return result, partResult.Err
		}
		if strings.TrimSpace(partResult.Result.ETag) == "" {
			return result, fmt.Errorf("OSS returned an empty ETag for part %d", partResult.Job.PartNumber)
		}
		parts = append(parts, oss.UploadPart{PartNumber: partResult.Job.PartNumber, ETag: partResult.Result.ETag})
	}
	body, err := completeMultipart(ctx, pool, selection.Paths, imur, parts, &params, sequential)
	if err != nil {
		return result, err
	}
	if err := parseUploadCallback(body, params.SHA1); err != nil {
		return result, err
	}
	options.reportBytes(fileSize, fileSize)
	completed = true
	result.BytesUploaded = fileSize
	result.Duration = time.Since(started)
	return result, nil
}
