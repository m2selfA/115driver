package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// UploadFile performs a rapid-upload negotiation and, when ResumePath is set,
// persists enough non-credential state to continue an interrupted OSS multipart
// upload in a later process. STS credentials are always fetched fresh and are
// never written to disk.
func UploadFile(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options) (Result, error) {
	if strings.TrimSpace(options.ResumePath) == "" {
		return uploadFileWithoutResume(ctx, client, dirID, fileName, fileSize, file, options)
	}
	return uploadFileResumable(ctx, client, dirID, fileName, fileSize, file, options)
}

func uploadFileResumable(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options) (Result, error) {
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
	if options.HealthTracker == nil {
		options.HealthTracker = transfer.NewDefaultNetworkHealthTracker()
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
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek upload file before digest: %w", err)
	}
	digest, err := client.GetDigestResult(file)
	if err != nil {
		return result, fmt.Errorf("calculate upload digest: %w", err)
	}
	if digest.Size != fileSize {
		return result, fmt.Errorf("upload file size changed during preparation: stat=%d digest=%d", fileSize, digest.Size)
	}
	result.SHA1 = digest.QuickID

	state, err := loadUploadResume(options.ResumePath, dirID, fileName, fileSize, digest.QuickID)
	if err != nil {
		return result, err
	}
	if state != nil {
		present, checkErr := uploadTargetAlreadyPresent(client, dirID, fileName, fileSize, digest.QuickID)
		if checkErr != nil {
			return result, fmt.Errorf("verify resumable upload target: %w", checkErr)
		}
		if present {
			state.Phase = uploadResumePhaseCompleted
			if err := saveUploadResume(options.ResumePath, *state); err != nil {
				return result, err
			}
			result.Resumed = true
			result.BytesUploaded = fileSize
			result.Duration = time.Since(started)
			return result, nil
		}
		if state.Phase == uploadResumePhaseCompleted {
			resetUploadResumeToPrepared(state)
			if err := saveUploadResume(options.ResumePath, *state); err != nil {
				return result, err
			}
		}
	}
	if state == nil {
		prepared := newPreparedUploadResume(dirID, fileName, fileSize, digest.QuickID)
		state = &prepared
		if err := saveUploadResume(options.ResumePath, *state); err != nil {
			return result, err
		}
	}

	if state.Phase == uploadResumePhaseMultipart {
		result.Resumed = true
		return resumeExistingMultipart(ctx, client, dirID, fileName, fileSize, file, options, state, started, result)
	}

	if options.Progress != nil {
		options.Progress("Checking 115 rapid upload...")
	}
	fastInfo, err := client.RapidUpload(digest.Size, fileName, dirID, digest.PreID, digest.QuickID, file)
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
		state.Phase = uploadResumePhaseCompleted
		if err := saveUploadResume(options.ResumePath, *state); err != nil {
			return result, err
		}
		result.Rapid = true
		result.BytesUploaded = fileSize
		result.Duration = time.Since(started)
		return result, nil
	}

	params := fastInfo.UploadOSSParams
	if params.SHA1 == "" {
		params.SHA1 = digest.QuickID
	}
	if !strings.EqualFold(params.SHA1, digest.QuickID) {
		return result, errors.New("115 upload initialization returned a different file SHA1")
	}
	endpoint := client.GetOSSEndpoint(client.UseInternalUpload)
	selection, err := resolveUploadPathsForParams(ctx, options, endpoint, params)
	if err != nil {
		return result, err
	}
	result.NetworkPaths = append([]transfer.NetworkPath(nil), selection.Paths...)

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek upload file for OSS transfer: %w", err)
	}
	if fileSize <= 1024 {
		pool := newOSSBucketPool(client, endpoint, params.Bucket)
		defer pool.close()
		if err := putObjectBound(ctx, pool, selection.Paths[0], file, fileSize, &params); err != nil {
			return result, err
		}
		if err := client.CheckUploadStatus(dirID, params.SHA1); err != nil {
			return result, err
		}
		state.Phase = uploadResumePhaseCompleted
		if err := saveUploadResume(options.ResumePath, *state); err != nil {
			return result, err
		}
		result.BytesUploaded = fileSize
		result.Duration = time.Since(started)
		return result, nil
	}

	jobs, effectiveChunkSize, err := buildUploadPartJobs(fileSize, options.ChunkSize)
	if err != nil {
		return result, err
	}
	sequential := len(selection.Paths) == 1
	pool := newOSSBucketPool(client, endpoint, params.Bucket)
	defer pool.close()
	imur, err := initiateMultipart(ctx, pool, selection.Paths, params.Object, sequential)
	if err != nil {
		return result, err
	}
	state.setMultipart(endpoint, params, imur.UploadID, effectiveChunkSize, sequential)
	if err := saveUploadResume(options.ResumePath, *state); err != nil {
		_ = abortMultipart(pool, selection.Paths, imur)
		return result, err
	}
	return runResumableMultipart(ctx, client, dirID, fileName, fileSize, file, options, state, selection.Paths, pool, imur, jobs, nil, started, result)
}

func resumeExistingMultipart(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options, state *uploadResumeState, started time.Time, result Result) (Result, error) {
	if strings.TrimSpace(state.Endpoint) == "" || strings.TrimSpace(state.Params.Bucket) == "" || strings.TrimSpace(state.Params.Object) == "" || strings.TrimSpace(state.UploadID) == "" || state.ChunkSize < MinPartSize {
		return result, fmt.Errorf("%w: multipart state is incomplete", ErrUploadResumeState)
	}
	params := state.uploadParams()
	selection, err := resolveUploadPathsForParams(ctx, options, state.Endpoint, params)
	if err != nil {
		return result, err
	}
	if state.Sequential && len(selection.Paths) > 1 {
		selection.Paths = selection.Paths[:1]
	}
	result.NetworkPaths = append([]transfer.NetworkPath(nil), selection.Paths...)
	result.Multipart = true
	result.Sequential = state.Sequential
	result.ChunkSize = state.ChunkSize

	jobs, effectiveChunkSize, err := buildUploadPartJobs(fileSize, state.ChunkSize)
	if err != nil {
		return result, err
	}
	if effectiveChunkSize != state.ChunkSize {
		return result, fmt.Errorf("%w: stored chunk size %d no longer describes the file", ErrUploadResumeState, state.ChunkSize)
	}
	result.PartCount = len(jobs)
	imur := oss.InitiateMultipartUploadResult{Bucket: params.Bucket, Key: params.Object, UploadID: state.UploadID}
	pool := newOSSBucketPool(client, state.Endpoint, params.Bucket)
	defer pool.close()
	listed, err := listUploadedParts(ctx, pool, selection.Paths, imur)
	if errors.Is(err, ErrUploadMultipartGone) {
		present, checkErr := waitForUploadedTarget(ctx, client, dirID, fileName, fileSize, state.SHA1)
		if checkErr != nil {
			return result, errors.Join(err, checkErr)
		}
		if present {
			state.Phase = uploadResumePhaseCompleted
			if saveErr := saveUploadResume(options.ResumePath, *state); saveErr != nil {
				return result, saveErr
			}
			result.BytesUploaded = fileSize
			result.Duration = time.Since(started)
			return result, nil
		}
		resetUploadResumeToPrepared(state)
		if saveErr := saveUploadResume(options.ResumePath, *state); saveErr != nil {
			return result, errors.Join(err, saveErr)
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return result, seekErr
		}
		return UploadFile(ctx, client, dirID, fileName, fileSize, file, options)
	}
	if err != nil {
		return result, err
	}
	existing, missing, err := reconcileUploadedParts(jobs, listed)
	if err != nil {
		return result, err
	}
	if state.Sequential {
		if err := validateSequentialResumeParts(existing); err != nil {
			return result, err
		}
	}
	result.ResumedParts = len(existing)
	return runResumableMultipart(ctx, client, dirID, fileName, fileSize, file, options, state, selection.Paths, pool, imur, missing, existing, started, result)
}

func runResumableMultipart(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options, state *uploadResumeState, paths []transfer.NetworkPath, pool *ossBucketPool, imur oss.InitiateMultipartUploadResult, jobs []transfer.UploadPartJob, existing []oss.UploadPart, started time.Time, result Result) (Result, error) {
	result.Multipart = true
	result.Sequential = state.Sequential
	result.ChunkSize = state.ChunkSize
	allJobs, _, err := buildUploadPartJobs(fileSize, state.ChunkSize)
	if err != nil {
		return result, err
	}
	result.PartCount = len(allJobs)

	report, scheduleErr := transfer.ScheduleUploadParts(ctx, paths, jobs, func(ctx context.Context, path transfer.NetworkPath, job transfer.UploadPartJob) (transfer.UploadPartResult, error) {
		return uploadPartWithRefresh(ctx, pool, imur, file, path, job)
	}, transfer.WithUploadPartRetries(options.Retries), transfer.WithUploadPartHealthTracker(options.HealthTracker), transfer.WithUploadPartPreserveOrder(state.Sequential))
	if scheduleErr != nil {
		// ResumePath is set, so deliberately keep the UploadID alive. A later
		// invocation reconciles server-side parts with ListUploadedParts.
		return result, scheduleErr
	}
	parts := append([]oss.UploadPart(nil), existing...)
	for _, partResult := range report.Results {
		if partResult.Err != nil {
			return result, partResult.Err
		}
		if strings.TrimSpace(partResult.Result.ETag) == "" {
			return result, fmt.Errorf("OSS returned an empty ETag for part %d", partResult.Job.PartNumber)
		}
		parts = append(parts, oss.UploadPart{PartNumber: partResult.Job.PartNumber, ETag: partResult.Result.ETag})
	}
	body, err := completeMultipart(ctx, pool, paths, imur, parts, ptrUploadParams(state.uploadParams()), state.Sequential)
	if err != nil {
		return result, err
	}
	if err := parseUploadCallback(body, state.SHA1); err != nil {
		return result, err
	}
	state.Phase = uploadResumePhaseCompleted
	if err := saveUploadResume(options.ResumePath, *state); err != nil {
		return result, err
	}
	result.BytesUploaded = fileSize
	result.Duration = time.Since(started)
	return result, nil
}

func resolveUploadPathsForParams(ctx context.Context, options Options, endpoint string, params driver.UploadOSSParams) (pathSelection, error) {
	probeURL, err := buildOSSProbeURL(endpoint, params.Bucket)
	if err != nil {
		return pathSelection{}, err
	}
	selection, err := resolveUploadPaths(ctx, options.Interfaces, probeURL)
	if err != nil {
		return pathSelection{}, err
	}
	if options.Progress != nil {
		if selection.Warning != nil {
			options.Progress(fmt.Sprintf("Network warning: %v", selection.Warning))
		}
		options.Progress(fmt.Sprintf("Using %d interface(s) for OSS upload...", len(selection.Paths)))
	}
	return selection, nil
}

func reconcileUploadedParts(jobs []transfer.UploadPartJob, listed []listedUploadPart) ([]oss.UploadPart, []transfer.UploadPartJob, error) {
	jobByNumber := make(map[int]transfer.UploadPartJob, len(jobs))
	for _, job := range jobs {
		jobByNumber[job.PartNumber] = job
	}
	existing := make([]oss.UploadPart, 0, len(listed))
	seen := make(map[int]struct{}, len(listed))
	for _, item := range listed {
		job, ok := jobByNumber[item.Part.PartNumber]
		if !ok || item.Size != job.Size || strings.TrimSpace(item.Part.ETag) == "" {
			return nil, nil, fmt.Errorf("%w: OSS part %d does not match the stored multipart layout", ErrUploadResumeState, item.Part.PartNumber)
		}
		if _, duplicate := seen[item.Part.PartNumber]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate OSS part %d", ErrUploadResumeState, item.Part.PartNumber)
		}
		seen[item.Part.PartNumber] = struct{}{}
		existing = append(existing, item.Part)
	}
	missing := make([]transfer.UploadPartJob, 0, len(jobs)-len(existing))
	for _, job := range jobs {
		if _, ok := seen[job.PartNumber]; !ok {
			missing = append(missing, job)
		}
	}
	return existing, missing, nil
}

func validateSequentialResumeParts(parts []oss.UploadPart) error {
	ordered := append([]oss.UploadPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PartNumber < ordered[j].PartNumber })
	for i, part := range ordered {
		if part.PartNumber != i+1 {
			return fmt.Errorf("%w: sequential multipart has a gap before part %d", ErrUploadResumeState, part.PartNumber)
		}
	}
	return nil
}

func waitForUploadedTarget(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, sha1 string) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		present, err := uploadTargetAlreadyPresent(client, dirID, fileName, fileSize, sha1)
		if err != nil || present {
			return present, err
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	return false, nil
}

func uploadTargetAlreadyPresent(client *driver.Pan115Client, dirID, fileName string, fileSize int64, sha1 string) (bool, error) {
	files, err := client.List(dirID)
	if err != nil {
		return false, err
	}
	for _, item := range *files {
		if item.IsDirectory || item.Name != fileName || item.Size != fileSize {
			continue
		}
		if strings.EqualFold(item.Sha1, sha1) {
			return true, nil
		}
	}
	return false, nil
}

func resetUploadResumeToPrepared(state *uploadResumeState) {
	if state == nil {
		return
	}
	state.Phase = uploadResumePhasePrepared
	state.Endpoint = ""
	state.Params = uploadResumeParams{}
	state.UploadID = ""
	state.ChunkSize = 0
	state.Sequential = false
}

func ptrUploadParams(params driver.UploadOSSParams) *driver.UploadOSSParams { return &params }
