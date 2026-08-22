package cmd

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type transferSessionPlan struct {
	Enabled         bool   `json:"enabled"`
	Managed         bool   `json:"managed"`
	Path            string `json:"path,omitempty"`
	PartsDir        string `json:"parts_dir,omitempty"`
	Exists          bool   `json:"exists"`
	LegacyPath      string `json:"legacy_path,omitempty"`
	LegacyAvailable bool   `json:"legacy_available,omitempty"`
}

type uploadTransferPlan struct {
	Operation               string              `json:"operation"`
	DryRun                  bool                `json:"dry_run"`
	LocalPath               string              `json:"local_path"`
	RemoteDir               string              `json:"remote_dir"`
	Destination             string              `json:"destination"`
	Kind                    string              `json:"kind"`
	Contents                bool                `json:"contents,omitempty"`
	Files                   int                 `json:"files"`
	Directories             int                 `json:"directories"`
	Size                    int64               `json:"size"`
	RemoteRootAction        string              `json:"remote_root_action"`
	RemoteDirectoriesCreate int                 `json:"remote_directories_create"`
	RemoteDirectoriesReuse  int                 `json:"remote_directories_reuse"`
	VerifyCandidates        int                 `json:"verify_candidates"`
	Resume                  bool                `json:"resume"`
	Session                 transferSessionPlan `json:"session"`
	InterfaceSelector       string              `json:"interface_selector"`
	WorkersPerInterface     int                 `json:"workers_per_interface"`
	ChunkSize               int64               `json:"chunk_size"`
	NetworkProbe            string              `json:"network_probe"`
}

type downloadTransferPlan struct {
	Operation              string              `json:"operation"`
	DryRun                 bool                `json:"dry_run"`
	RemotePath             string              `json:"remote_path"`
	LocalPath              string              `json:"local_path"`
	Kind                   string              `json:"kind"`
	Files                  int                 `json:"files"`
	Directories            int                 `json:"directories"`
	Size                   int64               `json:"size"`
	LocalRootAction        string              `json:"local_root_action"`
	LocalDirectoriesCreate int                 `json:"local_directories_create"`
	LocalDirectoriesReuse  int                 `json:"local_directories_reuse"`
	ExistingFiles          int                 `json:"existing_files"`
	Resume                 bool                `json:"resume"`
	Session                transferSessionPlan `json:"session"`
	Strategy               string              `json:"strategy"`
	InterfaceSelector      string              `json:"interface_selector"`
	WorkersPerInterface    int                 `json:"workers_per_interface"`
	ChunkSize              int64               `json:"chunk_size"`
	NetworkProbe           string              `json:"network_probe"`
}

type uploadPlanClient interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
}

type uploadRemoteTreePlan struct {
	Destination       string
	RootAction        string
	DirectoriesCreate int
	DirectoriesReuse  int
	VerifyCandidates  int
}

type remotePlannedDirectory struct {
	ID      string
	Planned bool
}

func runUploadDryRun(cmd *cobra.Command, args []string, jobs int) error {
	sources := args[:len(args)-1]
	remoteDir := args[len(args)-1]
	if len(sources) == 1 && jobs > 1 {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 is only valid for multi-source uploads"}
	}
	if len(sources) > 1 {
		if err := validateBatchUploadGlobalSources(sources); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	parallelism := jobs
	if parallelism > len(sources) {
		parallelism = len(sources)
	}
	workerLimit := 0
	var err error
	if parallelism > 1 {
		workerLimit, err = resolveBatchTransferWorkerLimit(cmd, parallelism, uploadWorkersPerInterface)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}

	if len(sources) == 1 {
		plan, err := buildUploadTransferPlan(cmd, client, sources[0], remoteDir, workerLimit)
		if err != nil {
			return err
		}
		printer.PrintSuccess(plan)
		printUploadTransferPlan(plan)
		return nil
	}

	continueOnError := batchContinueOnError(cmd)
	plans := make([]uploadTransferPlan, 0, len(sources))
	items := make([]batchItemResult, 0, len(sources))
	for i, source := range sources {
		plan, err := buildUploadTransferPlan(cmd, client, source, remoteDir, workerLimit)
		if err != nil {
			itemData := map[string]interface{}{"dry_run": true, "local_path": source, "remote_dir": remoteDir}
			items = append(items, failedBatchItem(source, itemData, err))
			printBatchItemFailure(i, len(sources), "plan upload "+source, err)
			if !continueOnError {
				break
			}
			continue
		}
		plans = append(plans, plan)
		items = append(items, successfulBatchItem(source, plan))
		printUploadTransferPlan(plan)
	}
	base := map[string]interface{}{
		"dry_run": true, "operation": "upload", "remote_dir": remoteDir, "plans": plans, "jobs": parallelism,
	}
	if workerLimit > 0 {
		base["workers_per_interface_per_job"] = workerLimit
	}
	data := batchResultData(len(sources), items, base)
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("upload dry-run", len(sources), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		fmt.Printf("DRY-RUN upload plan complete: %d source(s) -> %s; no data was transferred.\n", len(plans), remoteDir)
	}
	return nil
}

func buildUploadTransferPlan(cmd *cobra.Command, planClient uploadPlanClient, localPath, remoteDir string, workerLimit int) (uploadTransferPlan, error) {
	plan := uploadTransferPlan{Operation: "upload", DryRun: true, LocalPath: localPath, RemoteDir: remoteDir, NetworkProbe: "deferred"}
	if planClient == nil {
		return plan, &exitError{code: output.ExitError, msg: "upload plan client is nil"}
	}
	remoteRootID, err := resolver.ResolveDir(planClient, remoteDir)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve remote directory %s: %v", remoteDir, err)}
	}
	config, options, err := resolveUploadCommandOptions(cmd, workerLimit)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	plan.InterfaceSelector = options.Interfaces
	plan.WorkersPerInterface = options.WorkersPerInterface
	plan.ChunkSize = options.ChunkSize
	plan.Resume = config.Resume
	info, err := os.Lstat(localPath)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot stat local path: %v", err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return plan, &exitError{code: output.ExitArgs, msg: "Symbolic-link upload sources are not supported"}
	}
	if info.IsDir() {
		if !uploadRecursive {
			return plan, &exitError{code: output.ExitArgs, msg: "Local path is a directory; use --recursive to upload it"}
		}
		contents := uploadContents || uploadSourceRequestsContents(localPath)
		tree, err := scanLocalUploadTree(localPath)
		if err != nil {
			return plan, uploadPlanError(err)
		}
		remotePlan, err := inspectRecursiveUploadPlan(planClient, remoteDir, remoteRootID, tree, contents)
		if err != nil {
			return plan, uploadPlanError(err)
		}
		plan.Kind = "directory"
		plan.Contents = contents
		plan.LocalPath = tree.Root
		plan.Destination = remotePlan.Destination
		plan.RemoteRootAction = remotePlan.RootAction
		plan.RemoteDirectoriesCreate = remotePlan.DirectoriesCreate
		plan.RemoteDirectoriesReuse = remotePlan.DirectoriesReuse
		plan.VerifyCandidates = remotePlan.VerifyCandidates
		plan.Files = len(tree.Files)
		if len(tree.Directories) > 0 {
			plan.Directories = len(tree.Directories) - 1
		}
		for _, file := range tree.Files {
			if file.Size > 0 {
				plan.Size += file.Size
			}
		}
		mode := "directory"
		if contents {
			mode = "contents"
		}
		plan.Session, err = previewTransferSession("upload", "tree", tree.Root, plan.Destination, "multipart", mode, uploadSession, config.Resume)
		if err != nil {
			return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		return plan, nil
	}
	if uploadContents {
		return plan, &exitError{code: output.ExitArgs, msg: "--contents is only valid for recursive directory uploads"}
	}
	if !info.Mode().IsRegular() {
		return plan, &exitError{code: output.ExitArgs, msg: "Local upload source must be a regular file"}
	}
	plan.Kind = "file"
	plan.Files = 1
	plan.Size = info.Size()
	plan.Destination = pathpkg.Join(remoteDir, filepath.Base(localPath))
	if strings.HasPrefix(remoteDir, "/") && !strings.HasPrefix(plan.Destination, "/") {
		plan.Destination = "/" + plan.Destination
	}
	plan.RemoteRootAction = "reuse-directory"
	entries, err := listUploadPlanDirectory(planClient, remoteRootID, remoteDir)
	if err != nil {
		return plan, &exitError{code: output.ExitError, msg: err.Error()}
	}
	for _, entry := range entries {
		if !entry.IsDirectory && entry.Name == filepath.Base(localPath) && entry.Size == info.Size() {
			plan.VerifyCandidates++
		}
	}
	plan.Session, err = previewTransferSession("upload", "file", localPath, remoteDir, "multipart", "single-file", uploadSession, config.Resume)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return plan, nil
}

func inspectRecursiveUploadPlan(client uploadPlanClient, remoteDir, rootID string, tree localUploadTree, contents bool) (uploadRemoteTreePlan, error) {
	result := uploadRemoteTreePlan{}
	destination, err := recursiveUploadDestinationPath(remoteDir, tree.Root, contents)
	if err != nil {
		return result, err
	}
	result.Destination = destination
	directories := make(map[string]remotePlannedDirectory, len(tree.Directories))
	listingCache := make(map[string][]driver.File)
	if contents {
		directories[""] = remotePlannedDirectory{ID: rootID}
		result.RootAction = "reuse-contents-root"
	} else {
		entries, err := listUploadPlanDirectory(client, rootID, remoteDir)
		if err != nil {
			return result, err
		}
		base := pathpkg.Base(destination)
		childID, exists, err := findRequiredRemoteDirectory(entries, base, destination)
		if err != nil {
			return result, err
		}
		if exists {
			directories[""] = remotePlannedDirectory{ID: childID}
			result.RootAction = "reuse-directory"
			result.DirectoriesReuse++
		} else {
			directories[""] = remotePlannedDirectory{Planned: true}
			result.RootAction = "create-directory"
			result.DirectoriesCreate++
		}
	}

	for _, relative := range tree.Directories {
		if relative == "" {
			continue
		}
		parent := filepath.Dir(relative)
		if parent == "." {
			parent = ""
		}
		parentState, ok := directories[parent]
		if !ok {
			return result, fmt.Errorf("remote parent directory %q was not planned", parent)
		}
		if parentState.Planned {
			directories[relative] = remotePlannedDirectory{Planned: true}
			result.DirectoriesCreate++
			continue
		}
		entries, err := cachedUploadPlanListing(client, listingCache, parent, parentState.ID, pathpkg.Join(destination, filepath.ToSlash(parent)))
		if err != nil {
			return result, err
		}
		name := filepath.Base(relative)
		childID, exists, err := findRequiredRemoteDirectory(entries, name, pathpkg.Join(destination, filepath.ToSlash(relative)))
		if err != nil {
			return result, err
		}
		if exists {
			directories[relative] = remotePlannedDirectory{ID: childID}
			result.DirectoriesReuse++
		} else {
			directories[relative] = remotePlannedDirectory{Planned: true}
			result.DirectoriesCreate++
		}
	}

	for _, source := range tree.Files {
		parent := filepath.Dir(source.RelativePath)
		if parent == "." {
			parent = ""
		}
		state, ok := directories[parent]
		if !ok || state.Planned {
			continue
		}
		entries, err := cachedUploadPlanListing(client, listingCache, parent, state.ID, pathpkg.Join(destination, filepath.ToSlash(parent)))
		if err != nil {
			return result, err
		}
		name := filepath.Base(source.RelativePath)
		for _, entry := range entries {
			if !entry.IsDirectory && entry.Name == name && entry.Size == source.Size {
				result.VerifyCandidates++
				break
			}
		}
	}
	return result, nil
}

func cachedUploadPlanListing(client uploadPlanClient, cache map[string][]driver.File, relative, id, displayPath string) ([]driver.File, error) {
	if entries, ok := cache[relative]; ok {
		return entries, nil
	}
	entries, err := listUploadPlanDirectory(client, id, displayPath)
	if err != nil {
		return nil, err
	}
	cache[relative] = entries
	return entries, nil
}

func listUploadPlanDirectory(client uploadPlanClient, id, displayPath string) ([]driver.File, error) {
	entries, err := listRemoteDirectoryReadOnly(client, id)
	if err != nil {
		return nil, fmt.Errorf("list remote upload directory %q: %w", displayPath, err)
	}
	if entries == nil {
		return nil, fmt.Errorf("list remote upload directory %q returned nil", displayPath)
	}
	return append([]driver.File(nil), (*entries)...), nil
}

func findRequiredRemoteDirectory(entries []driver.File, name, destination string) (string, bool, error) {
	childID := ""
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		if !entry.IsDirectory {
			return "", false, fmt.Errorf("remote file %q conflicts with required upload directory", destination)
		}
		if childID != "" && childID != entry.FileID {
			return "", false, fmt.Errorf("remote directory %q is ambiguous", destination)
		}
		childID = entry.FileID
	}
	return childID, childID != "", nil
}

func uploadPlanError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errUploadUsage) {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return err
	}
	return &exitError{code: output.ExitError, msg: err.Error()}
}

func runDownloadDryRun(cmd *cobra.Command, args []string, jobs int) error {
	remotePaths := args[:len(args)-1]
	localTarget := args[len(args)-1]
	if len(remotePaths) == 1 && jobs > 1 {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 is only valid for multi-source downloads"}
	}
	if len(remotePaths) > 1 && strings.TrimSpace(downloadSession) != "" {
		return &exitError{code: output.ExitArgs, msg: "--session cannot be used with multiple download sources; managed sessions are created per source automatically"}
	}
	parallelism := jobs
	if parallelism > len(remotePaths) {
		parallelism = len(remotePaths)
	}
	workerLimit := 0
	var err error
	if parallelism > 1 {
		workerLimit, err = resolveBatchTransferWorkerLimit(cmd, parallelism, downloadWorkersPerInterface)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	if len(remotePaths) == 1 {
		plan, err := buildDownloadTransferPlan(cmd, client, remotePaths[0], localTarget, workerLimit)
		if err != nil {
			return err
		}
		printer.PrintSuccess(plan)
		printDownloadTransferPlan(plan)
		return nil
	}

	prepared, err := prepareBatchDownloadPlans(client, remotePaths, localTarget)
	if err != nil {
		return err
	}
	continueOnError := batchContinueOnError(cmd)
	plans := make([]downloadTransferPlan, 0, len(remotePaths))
	items := make([]batchItemResult, 0, len(remotePaths))
	for i, preparedPlan := range prepared {
		input := remotePaths[i]
		if preparedPlan.Err != nil {
			items = append(items, failedBatchItem(input, map[string]interface{}{"dry_run": true, "remote_path": input}, preparedPlan.Err))
			printBatchItemFailure(i, len(remotePaths), "plan download "+input, preparedPlan.Err)
			if !continueOnError {
				break
			}
			continue
		}
		plan, err := buildDownloadTransferPlan(cmd, client, preparedPlan.Source.RemotePath, preparedPlan.Source.LocalPath, workerLimit)
		if err != nil {
			items = append(items, failedBatchItem(input, map[string]interface{}{"dry_run": true, "remote_path": input, "local_path": preparedPlan.Source.LocalPath}, err))
			printBatchItemFailure(i, len(remotePaths), "plan download "+input, err)
			if !continueOnError {
				break
			}
			continue
		}
		plans = append(plans, plan)
		items = append(items, successfulBatchItem(input, plan))
		printDownloadTransferPlan(plan)
	}
	base := map[string]interface{}{
		"dry_run": true, "operation": "download", "local_dir": localTarget, "plans": plans, "jobs": parallelism,
	}
	if workerLimit > 0 {
		base["workers_per_interface_per_job"] = workerLimit
	}
	data := batchResultData(len(remotePaths), items, base)
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("download dry-run", len(remotePaths), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		fmt.Printf("DRY-RUN download plan complete: %d source(s) -> %s; no data was transferred.\n", len(plans), localTarget)
	}
	return nil
}

func buildDownloadTransferPlan(cmd *cobra.Command, planClient downloadCommandClient, remotePath, localTarget string, workerLimit int) (downloadTransferPlan, error) {
	plan := downloadTransferPlan{Operation: "download", DryRun: true, RemotePath: remotePath, NetworkProbe: "deferred"}
	if planClient == nil {
		return plan, &exitError{code: output.ExitError, msg: "download plan client is nil"}
	}
	options, err := resolveDownloadCommandOptions(cmd, workerLimit)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	plan.Strategy = options.Strategy
	plan.InterfaceSelector = options.Interfaces
	plan.WorkersPerInterface = options.WorkersPerInterface
	plan.ChunkSize = options.ChunkSize
	plan.Resume = options.Resume

	remoteID, isDirectory, err := resolver.ResolvePath(planClient, remotePath)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("cannot resolve remote path %s: %v", remotePath, err)}
	}
	if !isDirectory && strings.TrimSpace(options.SessionPath) != "" {
		return plan, &exitError{code: output.ExitArgs, msg: "--session is only used for recursive directory downloads"}
	}
	tree, err := collectRemoteDownloadTree(planClient, remoteID, remotePath, isDirectory, options.Recursive)
	if err != nil {
		if errors.Is(err, errDownloadUsage) {
			return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	plan.Files = len(tree.Files)
	if isDirectory && len(tree.Directories) > 0 {
		plan.Directories = len(tree.Directories) - 1
	}
	for _, file := range tree.Files {
		if file.File.Size > 0 {
			plan.Size += file.File.Size
		}
	}
	if isDirectory {
		plan.Kind = "directory"
		localPlan, err := inspectRecursiveDownloadLocalPlan(localTarget, tree)
		if err != nil {
			return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		plan.LocalPath = localPlan.Root
		plan.LocalRootAction = localPlan.RootAction
		plan.LocalDirectoriesCreate = localPlan.DirectoriesCreate
		plan.LocalDirectoriesReuse = localPlan.DirectoriesReuse
		plan.ExistingFiles = localPlan.ExistingFiles
		plan.Session, err = previewTransferSession("download", "tree", plan.LocalPath, remotePath, options.Strategy, "directory", options.SessionPath, options.Resume)
		if err != nil {
			return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		return plan, nil
	}

	plan.Kind = "file"
	if len(tree.Files) != 1 {
		return plan, &exitError{code: output.ExitError, msg: "resolved remote file produced an invalid plan"}
	}
	fileName := strings.TrimSpace(tree.Files[0].File.Name)
	if fileName == "" {
		fileName = pathpkg.Base(remotePath)
	}
	destination := resolver.ResolveLocalDownloadPath(localTarget, fileName)
	if err := validateDownloadFileDestination(destination); err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	plan.LocalPath = destination
	plan.LocalRootAction = "file-target"
	if info, err := os.Stat(destination); err == nil {
		if info.IsDir() {
			return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("download destination %q is a directory", destination)}
		}
		plan.ExistingFiles = 1
	} else if !os.IsNotExist(err) {
		return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot inspect download destination %q: %v", destination, err)}
	}
	plan.Session, err = previewTransferSession("download", "file", destination, remotePath, options.Strategy, "single-file", "", options.Resume)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	return plan, nil
}

func resolveDownloadCommandOptions(cmd *cobra.Command, workerLimit int) (downloadCommandOptions, error) {
	if err := validateDownloadTimeout(downloadTimeout); err != nil {
		return downloadCommandOptions{}, err
	}
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return downloadCommandOptions{}, err
	}
	if workerLimit > 0 {
		config.WorkersPerInterface = workerLimit
	} else {
		if commandFlagChanged(cmd, "workers-per-interface") {
			if downloadWorkersPerInterface <= 0 {
				return downloadCommandOptions{}, errors.New("--workers-per-interface must be > 0")
			}
			config.WorkersPerInterface = downloadWorkersPerInterface
		}
		config.WorkersPerInterface = applyParallelBatchWorkerLimit(config.WorkersPerInterface)
	}
	if strings.TrimSpace(downloadInterfaces) != "" {
		config.Interfaces = strings.TrimSpace(downloadInterfaces)
	}
	if strings.TrimSpace(downloadStrategy) != "" {
		config.Strategy = strings.ToLower(strings.TrimSpace(downloadStrategy))
	}
	chunkSizeText := config.ChunkSize
	if strings.TrimSpace(downloadChunkSize) != "" {
		chunkSizeText = strings.TrimSpace(downloadChunkSize)
	}
	chunkSize, err := transfer.ParseByteSize(chunkSizeText)
	if err != nil {
		return downloadCommandOptions{}, fmt.Errorf("invalid transfer chunk size %q: %v", chunkSizeText, err)
	}
	options := downloadCommandOptions{
		Recursive: downloadRecursive, Timeout: downloadTimeout, Interfaces: config.Interfaces, Strategy: config.Strategy,
		WorkersPerInterface: config.WorkersPerInterface, ProbeCacheTTL: config.ProbeCacheTTL, Retries: config.Retries,
		ChunkSize: chunkSize, HealthCooldown: config.HealthCooldown, HealthCooldownMax: config.HealthCooldownMax,
		Resume: config.Resume, SessionPath: downloadSession, URLRefreshes: config.URLRefreshes,
	}
	if err := validateDownloadCommandOptions(options); err != nil {
		return downloadCommandOptions{}, err
	}
	return options, nil
}

type downloadLocalPlan struct {
	Root              string
	RootAction        string
	DirectoriesCreate int
	DirectoriesReuse  int
	ExistingFiles     int
}

func inspectRecursiveDownloadLocalPlan(localTarget string, tree remoteDownloadTree) (downloadLocalPlan, error) {
	if localTarget == "" {
		localTarget = "."
	}
	result := downloadLocalPlan{Root: localTarget}
	if info, err := os.Stat(localTarget); err == nil {
		if !info.IsDir() {
			return result, fmt.Errorf("recursive download target %q is not a directory", localTarget)
		}
		result.RootAction = "reuse-directory"
		result.DirectoriesReuse++
	} else if os.IsNotExist(err) {
		if err := validateCreatableDownloadParent(filepath.Dir(localTarget)); err != nil {
			return result, err
		}
		result.RootAction = "create-directory"
		result.DirectoriesCreate++
	} else {
		return result, fmt.Errorf("cannot inspect recursive download target %q: %v", localTarget, err)
	}
	for _, relative := range tree.Directories {
		if relative == "" {
			continue
		}
		target := filepath.Join(localTarget, relative)
		if info, err := os.Stat(target); err == nil {
			if !info.IsDir() {
				return result, fmt.Errorf("local path %q conflicts with required download directory", target)
			}
			result.DirectoriesReuse++
		} else if os.IsNotExist(err) {
			result.DirectoriesCreate++
		} else {
			return result, fmt.Errorf("cannot inspect local download directory %q: %v", target, err)
		}
	}
	for _, source := range tree.Files {
		destination := filepath.Join(localTarget, source.RelativePath)
		if err := validateDownloadFileDestination(destination); err != nil {
			return result, err
		}
		if info, err := os.Stat(destination); err == nil {
			if info.IsDir() {
				return result, fmt.Errorf("download file %q conflicts with local directory %q", source.RemotePath, destination)
			}
			result.ExistingFiles++
		} else if !os.IsNotExist(err) {
			return result, fmt.Errorf("cannot inspect local download target %q: %v", destination, err)
		}
	}
	return result, nil
}

func validateDownloadFileDestination(destination string) error {
	if strings.TrimSpace(destination) == "" {
		return errors.New("download destination path is empty")
	}
	return validateCreatableDownloadParent(filepath.Dir(destination))
}

func validateCreatableDownloadParent(parent string) error {
	if parent == "" {
		parent = "."
	}
	current, err := filepath.Abs(parent)
	if err != nil {
		return err
	}
	for {
		info, statErr := os.Stat(current)
		if statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("download parent %q is not a directory", current)
			}
			return nil
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("cannot inspect download parent %q: %v", current, statErr)
		}
		next := filepath.Dir(current)
		if next == current {
			return nil
		}
		current = next
	}
}

func previewTransferSession(direction, kind, localAnchor, remoteTarget, strategy, transferMode, override string, resume bool) (transferSessionPlan, error) {
	plan := transferSessionPlan{Enabled: resume}
	if strings.TrimSpace(override) != "" && !resume {
		return plan, errors.New("--session requires transfer.resume=true")
	}
	if !resume {
		return plan, nil
	}
	if strings.TrimSpace(override) != "" {
		sessionPath, partsDir, err := deriveTransferSessionPaths(direction, localAnchor, remoteTarget, override)
		if err != nil {
			return plan, err
		}
		plan.Path = sessionPath
		plan.PartsDir = partsDir
		plan.Exists, err = pathExistsWithoutMutation(sessionPath)
		return plan, err
	}
	legacyPath, _, err := deriveTransferSessionPaths(direction, localAnchor, remoteTarget, "")
	if err != nil {
		return plan, err
	}
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return plan, err
	}
	profileName := auth.ResolveProfileName(configPath, profile)
	profileScope, err := transfer.SessionProfileScope(auth.ResolveConfigFilePath(configPath), profileName)
	if err != nil {
		return plan, err
	}
	identity, err := transfer.NewSessionIdentityV2(direction, kind, profileScope, localAnchor, remoteTarget, strategy, transferMode)
	if err != nil {
		return plan, err
	}
	localAbs, err := filepath.Abs(localAnchor)
	if err != nil {
		return plan, err
	}
	location, err := (transfer.SessionStore{Root: config.SessionDir}).Location(identity, filepath.Base(localAbs))
	if err != nil {
		return plan, err
	}
	plan.Managed = true
	plan.Path = location.PayloadPath
	plan.PartsDir = location.PartsDir
	plan.LegacyPath = legacyPath
	plan.Exists, err = pathExistsWithoutMutation(plan.Path)
	if err != nil {
		return plan, err
	}
	plan.LegacyAvailable, err = regularFileExistsWithoutMutation(legacyPath)
	if err != nil {
		return plan, err
	}
	return plan, nil
}

func pathExistsWithoutMutation(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func regularFileExistsWithoutMutation(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0, nil
}

func commandFlagChanged(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Lookup(name) != nil && cmd.Flags().Changed(name)
}

func printUploadTransferPlan(plan uploadTransferPlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN upload: %s -> %s (%s, %d file(s), %s)\n", plan.LocalPath, plan.Destination, plan.Kind, plan.Files, output.FormatFileSize(plan.Size))
	if plan.Kind == "directory" {
		fmt.Printf("  Remote directories: %d create, %d reuse; verify candidates: %d\n", plan.RemoteDirectoriesCreate, plan.RemoteDirectoriesReuse, plan.VerifyCandidates)
	} else if plan.VerifyCandidates > 0 {
		fmt.Printf("  Existing same-size candidate(s): %d; SHA1 verification would run before upload.\n", plan.VerifyCandidates)
	}
	printTransferSessionPlan(plan.Session)
}

func printDownloadTransferPlan(plan downloadTransferPlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN download: %s -> %s (%s, %d file(s), %s)\n", plan.RemotePath, plan.LocalPath, plan.Kind, plan.Files, output.FormatFileSize(plan.Size))
	if plan.Kind == "directory" {
		fmt.Printf("  Local directories: %d create, %d reuse; existing file target(s): %d\n", plan.LocalDirectoriesCreate, plan.LocalDirectoriesReuse, plan.ExistingFiles)
	} else if plan.ExistingFiles > 0 {
		fmt.Printf("  Existing local target will be replaced through resumable download semantics.\n")
	}
	printTransferSessionPlan(plan.Session)
}

func printTransferSessionPlan(plan transferSessionPlan) {
	if jsonOutput || !plan.Enabled {
		return
	}
	kind := "explicit"
	if plan.Managed {
		kind = "managed"
	}
	state := "new"
	if plan.Exists {
		state = "existing"
	} else if plan.LegacyAvailable {
		state = "legacy import available"
	}
	fmt.Printf("  Resume session: %s, %s, %s\n", kind, state, plan.Path)
}
