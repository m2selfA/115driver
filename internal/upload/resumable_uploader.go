package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/crypto/ec115"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// UploadFile performs a rapid-upload negotiation and, when ResumePath is set,
// persists enough non-credential state to continue an interrupted OSS multipart
// upload in a later process. STS credentials are always fetched fresh and are
// never written to disk.
func UploadFile(ctx context.Context, client *driver.Pan115Client, dirID, fileName string, fileSize int64, file *os.File, options Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	retries := options.Retries
	if retries < 0 {
		retries = 0
	}
	if options.Compatibility == nil {
		options.Compatibility = NewUploadCompatibilityState()
	}
	if options.Progress != nil {
		options.Progress("Preparing upload...")
	}
	options.reportBytes(0, fileSize)
	return runUploadWithRecovery(ctx, retries, options, fileSize, func(attempt int) (Result, error) {
		if attempt > 0 && file != nil {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return Result{}, fmt.Errorf("seek upload file before recovery retry: %w", err)
			}
		}
		attemptOptions := options
		attemptOptions.forceSequential = options.Compatibility.SequentialRequired()
		var result Result
		var err error
		if strings.TrimSpace(options.ResumePath) == "" {
			result, err = uploadFileWithoutResume(ctx, client, dirID, fileName, fileSize, file, attemptOptions)
		} else {
			result, err = uploadFileResumable(ctx, client, dirID, fileName, fileSize, file, attemptOptions)
		}
		if isUploadVerificationFailure(err) {
			options.Compatibility.RequireSequential()
		}
		return result, err
	}, waitUploadRecoveryBackoff)
}

type uploadRecoveryAttempt func(attempt int) (Result, error)
type uploadRecoveryWait func(context.Context, int) error

func runUploadWithRecovery(ctx context.Context, retries int, options Options, fileSize int64, attempt uploadRecoveryAttempt, wait uploadRecoveryWait) (Result, error) {
	var lastResult Result
	for attemptIndex := 0; ; attemptIndex++ {
		result, err := attempt(attemptIndex)
		lastResult = result
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, errors.Join(err, ctx.Err())
		}
		if attemptIndex >= retries || !isRecoverableUploadFailure(err) {
			return result, err
		}
		retryNumber := attemptIndex + 1
		if options.Progress != nil {
			mode := ""
			if isUploadVerificationFailure(err) {
				mode = " in sequential compatibility mode with interface failover"
			}
			options.Progress(fmt.Sprintf("Recovering upload; retry %d/%d%s after: %s", retryNumber, retries, mode, compactUploadRecoveryError(err)))
		}
		options.reportBytes(0, fileSize)
		if wait != nil {
			if waitErr := wait(ctx, retryNumber); waitErr != nil {
				return lastResult, errors.Join(err, waitErr)
			}
		}
	}
}

func waitUploadRecoveryBackoff(ctx context.Context, retryNumber int) error {
	if retryNumber < 1 {
		retryNumber = 1
	}
	shift := retryNumber - 1
	if shift > 3 {
		shift = 3
	}
	delay := time.Second * time.Duration(1<<shift)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func IsRecoverableError(err error) bool {
	return isRecoverableUploadFailure(err)
}

func isRecoverableUploadFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if isUploadVerificationFailure(err) || errors.Is(err, driver.ErrUploadFailed) || errors.Is(err, ec115.ErrInvalidCiphertext) ||
		errors.Is(err, transfer.ErrNetworkPathFailure) || errors.Is(err, transfer.ErrUploadPartScheduleIncomplete) || errors.Is(err, ErrUploadMultipartGone) ||
		errors.Is(err, context.DeadlineExceeded) || isOSSAuthError(err) || isOSSNoSuchUpload(err) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	if status := ossServiceStatus(err); status == 408 || status == 429 || status >= 500 {
		return true
	}
	return false
}

func isUploadVerificationFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrUploadVerificationFailed) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, `"code":10002`) ||
		(strings.Contains(message, "校验文件失败") && strings.Contains(message, "重新上传"))
}

func ossServiceStatus(err error) int {
	var serviceError oss.ServiceError
	if errors.As(err, &serviceError) {
		return serviceError.StatusCode
	}
	var serviceErrorPtr *oss.ServiceError
	if errors.As(err, &serviceErrorPtr) && serviceErrorPtr != nil {
		return serviceErrorPtr.StatusCode
	}
	return 0
}

func compactUploadRecoveryError(err error) string {
	message := strings.Join(strings.Fields(err.Error()), " ")
	const maxRunes = 120
	runes := []rune(message)
	if len(runes) <= maxRunes {
		return message
	}
	return string(runes[:maxRunes-1]) + "…"
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
	digest, err := resolveUploadDigest(file, fileSize, options.PreparedDigest)
	if err != nil {
		return result, err
	}
	result.SHA1 = digest.SHA1

	state, err := loadUploadResume(options.ResumePath, dirID, fileName, fileSize, digest.SHA1)
	if err != nil {
		return result, err
	}
	if state != nil {
		if state.ForceSequential {
			options.forceSequential = true
			if options.Compatibility != nil {
				options.Compatibility.RequireSequential()
			}
		}
		// After a 115 verification rejection, do not accept the old target as a
		// shortcut while the state is prepared for a forced clean re-upload.
		if !(state.ForceSequential && state.Phase == uploadResumePhasePrepared) {
			present, checkErr := uploadTargetAlreadyPresent(client, dirID, fileName, fileSize, digest.SHA1)
			if checkErr != nil {
				return result, fmt.Errorf("verify resumable upload target: %w", checkErr)
			}
			if present {
				state.Phase = uploadResumePhaseCompleted
				if err := saveUploadResume(options.ResumePath, *state); err != nil {
					return result, err
				}
				result.Resumed = true
				result.Verified = true
				result.Skipped = true
				options.reportBytes(fileSize, fileSize)
				result.BytesUploaded = 0
				result.Duration = time.Since(started)
				return result, nil
			}
		}
		if state.Phase == uploadResumePhaseCompleted {
			resetUploadResumeToPrepared(state)
			if err := saveUploadResume(options.ResumePath, *state); err != nil {
				return result, err
			}
		}
	}
	if state == nil {
		prepared := newPreparedUploadResume(dirID, fileName, fileSize, digest.SHA1)
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
		state.Phase = uploadResumePhaseCompleted
		if err := saveUploadResume(options.ResumePath, *state); err != nil {
			return result, err
		}
		options.reportBytes(fileSize, fileSize)
		result.Rapid = true
		result.BytesUploaded = fileSize
		result.Duration = time.Since(started)
		return result, nil
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
		options.reportBytes(fileSize, fileSize)
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
	sequential := options.forceSequential
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
	if migrateParallelSHA1ResumeToSequential(&options, state) {
		if saveErr := saveUploadResume(options.ResumePath, *state); saveErr != nil {
			return result, saveErr
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return result, seekErr
		}
		return uploadFileResumable(ctx, client, dirID, fileName, fileSize, file, options)
	}
	params := state.uploadParams()
	selection, err := resolveUploadPathsForParams(ctx, options, state.Endpoint, params)
	if err != nil {
		return result, err
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
			options.reportBytes(fileSize, fileSize)
			result.BytesUploaded = fileSize
			result.Duration = time.Since(started)
			return result, nil
		}
		prepareMissingMultipartRetry(&options, state)
		if saveErr := saveUploadResume(options.ResumePath, *state); saveErr != nil {
			return result, errors.Join(err, saveErr)
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return result, seekErr
		}
		return uploadFileResumable(ctx, client, dirID, fileName, fileSize, file, options)
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

	completedBytes := fileSize
	for _, job := range jobs {
		completedBytes -= job.Size
	}
	if completedBytes < 0 {
		completedBytes = 0
	}
	var progressBytes atomic.Int64
	progressBytes.Store(completedBytes)
	options.reportBytes(completedBytes, fileSize)
	report, scheduleErr := transfer.ScheduleUploadParts(ctx, paths, jobs, func(ctx context.Context, path transfer.NetworkPath, job transfer.UploadPartJob) (transfer.UploadPartResult, error) {
		partResult, partErr := uploadPartWithRefresh(ctx, pool, imur, file, path, job)
		if partErr == nil {
			options.reportBytes(progressBytes.Add(job.Size), fileSize)
		}
		return partResult, partErr
	}, transfer.WithUploadPartRetries(options.Retries), transfer.WithUploadPartWorkersPerInterface(options.WorkersPerInterface), transfer.WithUploadPartHealthTracker(options.HealthTracker), transfer.WithUploadPartPreserveOrder(state.Sequential))
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
		if isUploadVerificationFailure(err) {
			if resetErr := resetUploadResumeAfterVerificationFailure(options.ResumePath, state); resetErr != nil {
				return result, errors.Join(err, resetErr)
			}
		}
		return result, err
	}
	if err := parseUploadCallback(body, state.SHA1); err != nil {
		if isUploadVerificationFailure(err) {
			if resetErr := resetUploadResumeAfterVerificationFailure(options.ResumePath, state); resetErr != nil {
				return result, errors.Join(err, resetErr)
			}
		}
		return result, err
	}
	options.reportBytes(fileSize, fileSize)
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
	if options.Compatibility != nil {
		options.Compatibility.ObserveNetworkPaths(selection.Paths)
	}
	selection = applyUploadCompatibilitySelection(options, selection)
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

func migrateParallelSHA1ResumeToSequential(options *Options, state *uploadResumeState) bool {
	if state == nil || state.Sequential {
		return false
	}
	params := state.uploadParams()
	if !uploadCallbackRequiresSequentialSHA1(&params) {
		return false
	}
	requireSequentialUploadCompatibility(options, &params)
	state.ForceSequential = true
	resetUploadResumeToPrepared(state)
	return true
}

func prepareMissingMultipartRetry(options *Options, state *uploadResumeState) {
	if state == nil {
		return
	}
	// A non-sequential UploadID that disappeared without producing a valid 115
	// target is indistinguishable from the verification-failure state produced by
	// older builds. Prefer the protocol-compatible sequential path on retry.
	if !state.Sequential {
		state.ForceSequential = true
		if options != nil {
			options.forceSequential = true
			if options.Compatibility != nil {
				options.Compatibility.RequireSequential()
			}
		}
	}
	resetUploadResumeToPrepared(state)
}

func resetUploadResumeAfterVerificationFailure(path string, state *uploadResumeState) error {
	if state == nil {
		return errors.New("upload resume state is nil during verification recovery")
	}
	resetUploadResumeToPrepared(state)
	state.ForceSequential = true
	if strings.TrimSpace(path) == "" {
		return nil
	}
	// Keep only the stable file identity plus the compatibility latch. All stale
	// OSS multipart fields were cleared above, so the next attempt negotiates a
	// fresh object/upload ID and uses sequential OSS hashing.
	return saveUploadResume(path, *state)
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
