package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

var (
	errUploadUsage      = errors.New("invalid upload arguments")
	errUploadIncomplete = errors.New("recursive upload incomplete")
)

type uploadTreeClient interface {
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	Mkdir(parentID string, name string) (string, error)
}

type uploadPipelineDeps struct {
	uploadFile func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error)
}

func defaultUploadPipelineDeps() uploadPipelineDeps {
	return uploadPipelineDeps{uploadFile: uploadpkg.UploadFile}
}

type localUploadFile struct {
	FullPath        string
	RelativePath    string
	Size            int64
	ModTimeUnixNano int64
}

type localUploadTree struct {
	Root        string
	Directories []string
	Files       []localUploadFile
}

type uploadCommandFailure struct {
	RelativePath string
	Err          error
}

type uploadCommandSummary struct {
	LocalPath        string
	RemoteDir        string
	SessionPath      string
	FileCount        int
	SucceededCount   int
	FailedCount      int
	RapidCount       int
	ResumedCount     int
	TotalBytes       int64
	TransferredBytes int64
	Failures         []uploadCommandFailure
}

func executeRecursiveUpload(
	ctx context.Context,
	treeClient uploadTreeClient,
	uploadClient *driver.Pan115Client,
	localRoot, remoteDir, remoteRootID string,
	resume bool,
	sessionOverride string,
	options uploadpkg.Options,
	deps uploadPipelineDeps,
) (uploadCommandSummary, error) {
	summary := uploadCommandSummary{LocalPath: localRoot, RemoteDir: remoteDir}
	if ctx == nil {
		ctx = context.Background()
	}
	if treeClient == nil {
		return summary, errors.New("upload tree client is nil")
	}
	if deps.uploadFile == nil {
		return summary, errors.New("upload implementation is nil")
	}
	if strings.TrimSpace(sessionOverride) != "" && !resume {
		return summary, fmt.Errorf("%w: --session requires transfer.resume=true", errUploadUsage)
	}
	tree, err := scanLocalUploadTree(localRoot)
	if err != nil {
		return summary, err
	}
	summary.LocalPath = tree.Root
	summary.FileCount = len(tree.Files)
	for _, file := range tree.Files {
		summary.TotalBytes += file.Size
	}

	var session *transfer.TransferTreeSession
	partsDir := ""
	if resume {
		sessionPath, derivedPartsDir, err := deriveTransferSessionPaths("upload", tree.Root, remoteDir, sessionOverride)
		if err != nil {
			return summary, err
		}
		inside, err := pathIsWithin(tree.Root, sessionPath)
		if err != nil {
			return summary, err
		}
		if inside {
			return summary, fmt.Errorf("%w: recursive upload session must be outside the source directory", errUploadUsage)
		}
		inside, err = pathIsWithin(tree.Root, derivedPartsDir)
		if err != nil {
			return summary, err
		}
		if inside {
			return summary, fmt.Errorf("%w: recursive upload part state must be outside the source directory", errUploadUsage)
		}
		directories := make([]transfer.TransferTreeSessionDirectory, len(tree.Directories))
		for i, relative := range tree.Directories {
			directories[i] = transfer.TransferTreeSessionDirectory{RelativePath: relative}
		}
		files := make([]transfer.TransferTreeSessionFile, len(tree.Files))
		for i, file := range tree.Files {
			files[i] = transfer.TransferTreeSessionFile{
				RelativePath: file.RelativePath, Size: file.Size, ModTimeUnixNano: file.ModTimeUnixNano,
			}
		}
		session, err = transfer.OpenTransferTreeSession(sessionPath, transfer.TransferTreeSessionSpec{
			Direction: "upload", Source: tree.Root, Destination: remoteDir, Strategy: "multipart",
		}, directories, files)
		if err != nil {
			return summary, err
		}
		summary.SessionPath = sessionPath
		partsDir = derivedPartsDir
	}

	directoryIDs, listings, err := ensureRemoteUploadDirectories(treeClient, remoteRootID, tree.Directories, session)
	if err != nil {
		return summary, err
	}

	sessionFiles := map[string]transfer.TransferTreeSessionFile{}
	if session != nil {
		for _, file := range session.Snapshot().Files {
			sessionFiles[file.RelativePath] = file
		}
	}

	for _, source := range tree.Files {
		if err := ctx.Err(); err != nil {
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: source.RelativePath, Err: err})
			break
		}
		parentRelative := filepath.Dir(source.RelativePath)
		if parentRelative == "." {
			parentRelative = ""
		}
		parentID, ok := directoryIDs[parentRelative]
		if !ok {
			return summary, fmt.Errorf("remote parent for %q was not prepared", source.RelativePath)
		}

		if state, ok := sessionFiles[source.RelativePath]; ok && state.Completed {
			matches := remoteUploadFileExists(listings[parentRelative], filepath.Base(source.RelativePath), source.Size, state.SHA1)
			if matches {
				summary.SucceededCount++
				summary.ResumedCount++
				continue
			}
			if err := session.MarkFilePending(source.RelativePath, errors.New("previously completed remote file is no longer present")); err != nil {
				return summary, err
			}
		}

		info, err := os.Lstat(source.FullPath)
		if err != nil {
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: source.RelativePath, Err: err})
			if session != nil {
				_ = session.MarkFilePending(source.RelativePath, err)
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() != source.Size || info.ModTime().UnixNano() != source.ModTimeUnixNano {
			err := errors.New("local file changed after the recursive upload scan")
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: source.RelativePath, Err: err})
			if session != nil {
				_ = session.MarkFilePending(source.RelativePath, err)
			}
			continue
		}
		file, err := os.Open(source.FullPath)
		if err != nil {
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: source.RelativePath, Err: err})
			if session != nil {
				_ = session.MarkFilePending(source.RelativePath, err)
			}
			continue
		}
		fileOptions := options
		if session != nil {
			fileOptions.ResumePath = uploadResumePathForRelative(partsDir, source.RelativePath)
		}
		result, uploadErr := deps.uploadFile(ctx, uploadClient, parentID, filepath.Base(source.RelativePath), source.Size, file, fileOptions)
		closeErr := file.Close()
		if uploadErr == nil && closeErr != nil {
			uploadErr = closeErr
		}
		if result.SHA1 != "" && session != nil {
			if err := session.SetFileSHA1(source.RelativePath, result.SHA1); err != nil {
				return summary, err
			}
			sessionFiles[source.RelativePath] = transfer.TransferTreeSessionFile{
				RelativePath: source.RelativePath, Size: source.Size, ModTimeUnixNano: source.ModTimeUnixNano, SHA1: result.SHA1,
			}
		}
		if uploadErr != nil {
			summary.Failures = append(summary.Failures, uploadCommandFailure{RelativePath: source.RelativePath, Err: uploadErr})
			if session != nil {
				if err := session.MarkFilePending(source.RelativePath, uploadErr); err != nil {
					return summary, err
				}
			}
			continue
		}
		if session != nil {
			if err := session.MarkFileCompleted(source.RelativePath); err != nil {
				return summary, err
			}
		}
		summary.SucceededCount++
		summary.TransferredBytes += result.BytesUploaded
		if result.Rapid {
			summary.RapidCount++
		}
		if result.Resumed || result.ResumedParts > 0 {
			summary.ResumedCount++
		}
	}

	summary.FailedCount = summary.FileCount - summary.SucceededCount
	if summary.FailedCount == 0 && session != nil {
		if err := session.Remove(); err != nil {
			return summary, err
		}
		if err := safeRemoveUploadPartsDir(partsDir); err != nil {
			return summary, err
		}
		summary.SessionPath = ""
	}
	if summary.FailedCount > 0 {
		return summary, fmt.Errorf("%w: %d of %d files failed", errUploadIncomplete, summary.FailedCount, summary.FileCount)
	}
	return summary, nil
}

func scanLocalUploadTree(root string) (localUploadTree, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return localUploadTree{}, err
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil {
		return localUploadTree{}, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return localUploadTree{}, fmt.Errorf("%w: recursive upload source must be a real directory", errUploadUsage)
	}
	tree := localUploadTree{Root: absolute, Directories: []string{""}}
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == absolute {
			return nil
		}
		relative, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symbolic link %q is not allowed in recursive upload", errUploadUsage, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			tree.Directories = append(tree.Directories, relative)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: special file %q is not supported", errUploadUsage, relative)
		}
		tree.Files = append(tree.Files, localUploadFile{
			FullPath: path, RelativePath: relative, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return localUploadTree{}, err
	}
	sort.Slice(tree.Directories, func(i, j int) bool {
		leftDepth := strings.Count(tree.Directories[i], string(filepath.Separator))
		rightDepth := strings.Count(tree.Directories[j], string(filepath.Separator))
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return tree.Directories[i] < tree.Directories[j]
	})
	sort.Slice(tree.Files, func(i, j int) bool { return tree.Files[i].RelativePath < tree.Files[j].RelativePath })
	return tree, nil
}

func ensureRemoteUploadDirectories(client uploadTreeClient, rootID string, directories []string, session *transfer.TransferTreeSession) (map[string]string, map[string][]driver.File, error) {
	ids := map[string]string{"": rootID}
	listings := make(map[string][]driver.File, len(directories))
	for _, relative := range directories {
		if relative == "" {
			if session != nil {
				if err := session.SetDirectoryRemoteID("", rootID); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		parent := filepath.Dir(relative)
		if parent == "." {
			parent = ""
		}
		parentID, ok := ids[parent]
		if !ok {
			return nil, nil, fmt.Errorf("remote parent directory %q was not prepared", parent)
		}
		entries, err := client.List(parentID)
		if err != nil {
			return nil, nil, fmt.Errorf("list remote upload parent %q: %w", parent, err)
		}
		if entries == nil {
			return nil, nil, fmt.Errorf("list remote upload parent %q returned nil", parent)
		}
		listings[parent] = append([]driver.File(nil), (*entries)...)
		name := filepath.Base(relative)
		childID := ""
		for _, entry := range *entries {
			if entry.Name != name {
				continue
			}
			if !entry.IsDirectory {
				return nil, nil, fmt.Errorf("remote file %q conflicts with required upload directory", relative)
			}
			if childID != "" && childID != entry.FileID {
				return nil, nil, fmt.Errorf("remote directory %q is ambiguous", relative)
			}
			childID = entry.FileID
		}
		if childID == "" {
			childID, err = client.Mkdir(parentID, name)
			if err != nil {
				return nil, nil, fmt.Errorf("create remote upload directory %q: %w", relative, err)
			}
			listings[parent] = append(listings[parent], driver.File{FileID: childID, Name: name, IsDirectory: true})
		}
		ids[relative] = childID
		if session != nil {
			if err := session.SetDirectoryRemoteID(relative, childID); err != nil {
				return nil, nil, err
			}
		}
	}
	for relative, id := range ids {
		if _, ok := listings[relative]; ok {
			continue
		}
		entries, err := client.List(id)
		if err != nil {
			return nil, nil, fmt.Errorf("list remote upload directory %q: %w", relative, err)
		}
		if entries == nil {
			return nil, nil, fmt.Errorf("list remote upload directory %q returned nil", relative)
		}
		listings[relative] = append([]driver.File(nil), (*entries)...)
	}
	return ids, listings, nil
}

func remoteUploadFileExists(entries []driver.File, name string, size int64, sha1 string) bool {
	if strings.TrimSpace(sha1) == "" {
		return false
	}
	for _, entry := range entries {
		if entry.IsDirectory || entry.Name != name || entry.Size != size {
			continue
		}
		if strings.EqualFold(entry.Sha1, sha1) {
			return true
		}
	}
	return false
}

func safeRemoveUploadPartsDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || !strings.HasSuffix(base, ".parts") {
		return fmt.Errorf("refusing to remove unsafe upload part state directory %q", path)
	}
	return os.RemoveAll(path)
}
