package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type dryRunPlanClient struct {
	dirIDs        map[string]string
	lists         map[string][]driver.File
	files         map[string]driver.File
	downloadCalls int
}

func (client *dryRunPlanClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	id, ok := client.dirIDs[dir]
	if !ok {
		return nil, driver.ErrNotExist
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(id)}, nil
}

func (client *dryRunPlanClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *dryRunPlanClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := client.lists[dirID]
	if offset >= int64(len(entries)) {
		empty := []driver.File{}
		return &empty, nil
	}
	end := offset + limit
	if end > int64(len(entries)) {
		end = int64(len(entries))
	}
	page := append([]driver.File(nil), entries[offset:end]...)
	return &page, nil
}

func (client *dryRunPlanClient) GetFile(fileID string) (*driver.File, error) {
	file, ok := client.files[fileID]
	if !ok {
		return nil, errors.New("file missing")
	}
	copy := file
	return &copy, nil
}

func (client *dryRunPlanClient) Download(string) (*driver.DownloadInfo, error) {
	client.downloadCalls++
	return nil, errors.New("dry-run must not request a signed download URL")
}

func TestUploadDryRunPlansRecursiveCreateWithoutRemoteOrSessionWrites(t *testing.T) {
	oldConfigPath, oldProfile := configPath, profile
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	oldInterfaces, oldChunkSize, oldWorkers, oldTimeout := uploadInterfaces, uploadChunkSize, uploadWorkersPerInterface, uploadTimeout
	t.Cleanup(func() {
		configPath, profile = oldConfigPath, oldProfile
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
		uploadInterfaces, uploadChunkSize, uploadWorkersPerInterface, uploadTimeout = oldInterfaces, oldChunkSize, oldWorkers, oldTimeout
	})

	root := t.TempDir()
	configPath = filepath.Join(root, "missing-config.toml")
	profile = ""
	sessionRoot := filepath.Join(root, "session-store")
	t.Setenv("115DRIVER_SESSION_DIR", sessionRoot)
	uploadRecursive = true
	uploadContents = false
	uploadSession = ""
	uploadInterfaces = ""
	uploadChunkSize = ""
	uploadWorkersPerInterface = 0

	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.bin"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "b.bin"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &dryRunPlanClient{
		dirIDs: map[string]string{"remote": "rid"},
		lists:  map[string][]driver.File{"rid": {}},
		files:  map[string]driver.File{},
	}
	plan, err := buildUploadTransferPlan(&cobra.Command{}, client, source, "/remote", 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != "directory" || plan.Files != 2 || plan.Directories != 1 || plan.Size != 8 {
		t.Fatalf("unexpected upload plan counts: %#v", plan)
	}
	if plan.RemoteRootAction != "create-directory" || plan.RemoteDirectoriesCreate != 2 || plan.RemoteDirectoriesReuse != 0 {
		t.Fatalf("unexpected remote directory plan: %#v", plan)
	}
	if !plan.Resume || !plan.Session.Enabled || !plan.Session.Managed || plan.Session.Path == "" || plan.Session.Exists {
		t.Fatalf("unexpected upload session preview: %#v", plan.Session)
	}
	if _, err := os.Stat(sessionRoot); !os.IsNotExist(err) {
		t.Fatalf("upload dry-run created session store: %v", err)
	}
}

func TestDownloadDryRunDoesNotCreateDestinationSessionOrSignedURL(t *testing.T) {
	oldConfigPath, oldProfile := configPath, profile
	oldRecursive, oldSession := downloadRecursive, downloadSession
	oldInterfaces, oldStrategy, oldChunkSize, oldWorkers, oldTimeout := downloadInterfaces, downloadStrategy, downloadChunkSize, downloadWorkersPerInterface, downloadTimeout
	t.Cleanup(func() {
		configPath, profile = oldConfigPath, oldProfile
		downloadRecursive, downloadSession = oldRecursive, oldSession
		downloadInterfaces, downloadStrategy, downloadChunkSize, downloadWorkersPerInterface, downloadTimeout = oldInterfaces, oldStrategy, oldChunkSize, oldWorkers, oldTimeout
	})

	root := t.TempDir()
	configPath = filepath.Join(root, "missing-config.toml")
	profile = ""
	sessionRoot := filepath.Join(root, "session-store")
	t.Setenv("115DRIVER_SESSION_DIR", sessionRoot)
	downloadRecursive = true
	downloadSession = ""
	downloadInterfaces = ""
	downloadStrategy = ""
	downloadChunkSize = ""
	downloadWorkersPerInterface = 0

	client := &dryRunPlanClient{
		dirIDs: map[string]string{"photos": "d1"},
		lists: map[string][]driver.File{
			"d1": {
				{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 5},
				{FileID: "ds", Name: "sub", IsDirectory: true},
			},
			"ds": {{FileID: "fb", Name: "b.bin", PickCode: "pb", Size: 7}},
		},
		files: map[string]driver.File{},
	}
	localRoot := filepath.Join(root, "download-target")
	plan, err := buildDownloadTransferPlan(&cobra.Command{}, client, "/photos", localRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != "directory" || plan.Files != 2 || plan.Directories != 1 || plan.Size != 12 {
		t.Fatalf("unexpected download plan counts: %#v", plan)
	}
	if plan.LocalRootAction != "create-directory" || plan.LocalDirectoriesCreate != 2 || plan.LocalDirectoriesReuse != 0 {
		t.Fatalf("unexpected local directory plan: %#v", plan)
	}
	if client.downloadCalls != 0 {
		t.Fatalf("dry-run requested signed download URLs %d time(s)", client.downloadCalls)
	}
	if _, err := os.Stat(localRoot); !os.IsNotExist(err) {
		t.Fatalf("download dry-run created local destination: %v", err)
	}
	if _, err := os.Stat(sessionRoot); !os.IsNotExist(err) {
		t.Fatalf("download dry-run created session store: %v", err)
	}
}

func TestDownloadDryRunRejectsLocalDirectoryTypeConflict(t *testing.T) {
	oldConfigPath, oldProfile := configPath, profile
	oldRecursive, oldSession := downloadRecursive, downloadSession
	t.Cleanup(func() {
		configPath, profile = oldConfigPath, oldProfile
		downloadRecursive, downloadSession = oldRecursive, oldSession
	})
	root := t.TempDir()
	configPath = filepath.Join(root, "missing-config.toml")
	profile = ""
	t.Setenv("115DRIVER_SESSION_DIR", filepath.Join(root, "session-store"))
	downloadRecursive = true
	downloadSession = ""

	client := &dryRunPlanClient{
		dirIDs: map[string]string{"photos": "d1"},
		lists: map[string][]driver.File{
			"d1": {{FileID: "ds", Name: "sub", IsDirectory: true}},
			"ds": {{FileID: "fb", Name: "b.bin", PickCode: "pb", Size: 7}},
		},
		files: map[string]driver.File{},
	}
	localRoot := filepath.Join(root, "target")
	if err := os.MkdirAll(localRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "sub"), []byte("conflict"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := buildDownloadTransferPlan(&cobra.Command{}, client, "/photos", localRoot, 0)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || !strings.Contains(err.Error(), "conflicts with required download directory") {
		t.Fatalf("unexpected local conflict result: %T %v", err, err)
	}
	if client.downloadCalls != 0 {
		t.Fatalf("conflicting dry-run unexpectedly requested signed URLs: %d", client.downloadCalls)
	}
}

func TestUploadDryRunRejectsRemoteFileWhereDirectoryMustBeCreated(t *testing.T) {
	oldConfigPath, oldProfile := configPath, profile
	oldRecursive, oldContents, oldSession := uploadRecursive, uploadContents, uploadSession
	t.Cleanup(func() {
		configPath, profile = oldConfigPath, oldProfile
		uploadRecursive, uploadContents, uploadSession = oldRecursive, oldContents, oldSession
	})
	root := t.TempDir()
	configPath = filepath.Join(root, "missing-config.toml")
	profile = ""
	t.Setenv("115DRIVER_SESSION_DIR", filepath.Join(root, "session-store"))
	uploadRecursive = true
	uploadContents = false
	uploadSession = ""
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	client := &dryRunPlanClient{
		dirIDs: map[string]string{"remote": "rid"},
		lists: map[string][]driver.File{
			"rid": {{FileID: "conflict", Name: "source", Size: 1}},
		},
		files: map[string]driver.File{},
	}
	_, err := buildUploadTransferPlan(&cobra.Command{}, client, source, "/remote", 0)
	if err == nil || !strings.Contains(err.Error(), "conflicts with required upload directory") {
		t.Fatalf("unexpected remote conflict result: %v", err)
	}
}
