package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type recursiveUploadProgress struct {
	mu       sync.Mutex
	total    int64
	current  int64
	files    map[string]int64
	callback func(completed, total int64)
}

func newRecursiveUploadProgress(total int64, callback func(completed, total int64)) *recursiveUploadProgress {
	progress := &recursiveUploadProgress{total: total, files: make(map[string]int64), callback: callback}
	if callback != nil {
		callback(0, total)
	}
	return progress
}

func (progress *recursiveUploadProgress) set(relative string, completed, size int64) {
	if progress == nil || progress.callback == nil {
		return
	}
	if completed < 0 {
		completed = 0
	}
	if completed > size {
		completed = size
	}
	progress.mu.Lock()
	previous := progress.files[relative]
	progress.files[relative] = completed
	progress.current += completed - previous
	if progress.current < 0 {
		progress.current = 0
	}
	if progress.current > progress.total {
		progress.current = progress.total
	}
	current, total := progress.current, progress.total
	progress.mu.Unlock()
	progress.callback(current, total)
}

type recursiveUploadFileResult struct {
	Index            int
	RelativePath     string
	Success          bool
	Rapid            bool
	Resumed          bool
	TransferredBytes int64
	Err              error
	Fatal            error
}

type recursiveUploadFileProcessor struct {
	uploadClient *driver.Pan115Client
	deps         uploadPipelineDeps
	directoryIDs map[string]string
	listings     map[string][]driver.File
	session      *transfer.TransferTreeSession
	sessionFiles map[string]transfer.TransferTreeSessionFile
	partsDir     string
	options      uploadpkg.Options
	progress     *recursiveUploadProgress
	fileCount    int
}

func (processor *recursiveUploadFileProcessor) process(ctx context.Context, source localUploadFile, fileIndex int, interfaceSelector string) recursiveUploadFileResult {
	outcome := recursiveUploadFileResult{Index: fileIndex, RelativePath: source.RelativePath}
	if err := ctx.Err(); err != nil {
		outcome.Err = err
		return outcome
	}
	parentRelative := filepath.Dir(source.RelativePath)
	if parentRelative == "." {
		parentRelative = ""
	}
	parentID, ok := processor.directoryIDs[parentRelative]
	if !ok {
		outcome.Fatal = fmt.Errorf("remote parent for %q was not prepared", source.RelativePath)
		return outcome
	}

	if state, ok := processor.sessionFiles[source.RelativePath]; ok && state.Completed {
		if remoteUploadFileExists(processor.listings[parentRelative], filepath.Base(source.RelativePath), source.Size, state.SHA1) {
			processor.progress.set(source.RelativePath, source.Size, source.Size)
			outcome.Success = true
			outcome.Resumed = true
			return outcome
		}
		if err := processor.session.MarkFilePending(source.RelativePath, errors.New("previously completed remote file is no longer present")); err != nil {
			outcome.Fatal = err
			return outcome
		}
	}

	info, err := os.Lstat(source.FullPath)
	if err != nil {
		outcome.Err = err
		processor.progress.set(source.RelativePath, 0, source.Size)
		if processor.session != nil {
			_ = processor.session.MarkFilePending(source.RelativePath, err)
		}
		return outcome
	}
	if !info.Mode().IsRegular() || info.Size() != source.Size || info.ModTime().UnixNano() != source.ModTimeUnixNano {
		err := errors.New("local file changed after the recursive upload scan")
		outcome.Err = err
		processor.progress.set(source.RelativePath, 0, source.Size)
		if processor.session != nil {
			_ = processor.session.MarkFilePending(source.RelativePath, err)
		}
		return outcome
	}
	file, err := os.Open(source.FullPath)
	if err != nil {
		outcome.Err = err
		processor.progress.set(source.RelativePath, 0, source.Size)
		if processor.session != nil {
			_ = processor.session.MarkFilePending(source.RelativePath, err)
		}
		return outcome
	}

	fileOptions := processor.options
	if interfaceSelector != "" {
		fileOptions.Interfaces = interfaceSelector
		// A pinned recursive-file slot gets one path attempt. If that path
		// disappears, processConcurrent immediately retries the same resumable
		// file through the original selector, preserving the full retry budget
		// for healthy-path recovery instead of waiting on the dead NIC.
		fileOptions.Retries = 0
	}
	statusPrefix := fmt.Sprintf("[%d/%d] %s", fileIndex+1, processor.fileCount, source.RelativePath)
	if processor.options.Progress != nil {
		outerProgress := processor.options.Progress
		outerProgress(statusPrefix)
		fileOptions.Progress = func(message string) {
			outerProgress(statusPrefix + " — " + message)
		}
	}
	if processor.options.ProgressBytes != nil {
		fileOptions.ProgressBytes = func(completed, _ int64) {
			processor.progress.set(source.RelativePath, completed, source.Size)
		}
	}
	if processor.session != nil {
		fileOptions.ResumePath = uploadResumePathForRelative(processor.partsDir, source.RelativePath)
	}
	result, uploadErr := processor.deps.uploadFile(ctx, processor.uploadClient, parentID, filepath.Base(source.RelativePath), source.Size, file, fileOptions)
	closeErr := file.Close()
	if uploadErr == nil && closeErr != nil {
		uploadErr = closeErr
	}
	if result.SHA1 != "" && processor.session != nil {
		if err := processor.session.SetFileSHA1(source.RelativePath, result.SHA1); err != nil {
			outcome.Fatal = err
			return outcome
		}
	}
	if uploadErr != nil {
		processor.progress.set(source.RelativePath, 0, source.Size)
		if processor.session != nil {
			if err := processor.session.MarkFilePending(source.RelativePath, uploadErr); err != nil {
				outcome.Fatal = err
				return outcome
			}
		}
		outcome.Err = uploadErr
		return outcome
	}
	if processor.session != nil {
		if err := processor.session.MarkFileCompleted(source.RelativePath); err != nil {
			outcome.Fatal = err
			return outcome
		}
	}
	processor.progress.set(source.RelativePath, source.Size, source.Size)
	outcome.Success = true
	outcome.Rapid = result.Rapid
	outcome.Resumed = result.Resumed || result.ResumedParts > 0
	outcome.TransferredBytes = result.BytesUploaded
	return outcome
}

func (processor *recursiveUploadFileProcessor) processConcurrent(ctx context.Context, files []localUploadFile, startIndex int, paths []transfer.NetworkPath) ([]recursiveUploadFileResult, error) {
	if len(files) == 0 {
		return nil, nil
	}
	workersPerInterface := processor.options.WorkersPerInterface
	if workersPerInterface <= 0 {
		workersPerInterface = 1
	}
	if len(paths)*workersPerInterface < 2 {
		return nil, errors.New("concurrent recursive upload requires at least two connection slots")
	}
	type uploadJob struct {
		index  int
		source localUploadFile
	}
	jobs := make(chan uploadJob, len(files))
	results := make(chan recursiveUploadFileResult, len(files))
	for i, source := range files {
		jobs <- uploadJob{index: startIndex + i, source: source}
	}
	close(jobs)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	for _, networkPath := range paths {
		selector := networkPath.LocalIP.String()
		for slot := 0; slot < workersPerInterface; slot++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-workerCtx.Done():
						return
					case job, ok := <-jobs:
						if !ok {
							return
						}
						outcome := processor.process(workerCtx, job.source, job.index, selector)
						if outcome.Fatal != nil {
							results <- outcome
							cancel()
							return
						}
						if outcome.Err != nil && uploadpkg.IsRecoverableError(outcome.Err) && workerCtx.Err() == nil {
							// The per-file retry budget on the pinned path is exhausted. One
							// final pass through the original selector lets discovery move the
							// resumable file to another healthy NIC or another address.
							outcome = processor.process(workerCtx, job.source, job.index, "")
						}
						results <- outcome
					}
				}
			}()
		}
	}
	workers.Wait()
	close(results)

	outcomes := make([]recursiveUploadFileResult, 0, len(files))
	var fatal error
	for outcome := range results {
		outcomes = append(outcomes, outcome)
		if outcome.Fatal != nil && fatal == nil {
			fatal = outcome.Fatal
		}
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Index < outcomes[j].Index })
	if fatal != nil {
		return outcomes, fatal
	}
	if err := ctx.Err(); err != nil {
		return outcomes, err
	}
	return outcomes, nil
}

func applyRecursiveUploadFileResult(summary *uploadCommandSummary, outcome recursiveUploadFileResult) {
	if summary == nil {
		return
	}
	if !outcome.Success {
		if outcome.Err != nil {
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: outcome.RelativePath, Err: outcome.Err})
		}
		return
	}
	summary.SucceededCount++
	summary.TransferredBytes += outcome.TransferredBytes
	if outcome.Rapid {
		summary.RapidCount++
	}
	if outcome.Resumed {
		summary.ResumedCount++
	}
}
