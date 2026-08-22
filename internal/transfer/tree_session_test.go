package transfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTransferTreeSessionPersistsAndReconcilesFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	spec := TransferTreeSessionSpec{Direction: "upload", Source: "C:/src", Destination: "/remote", Strategy: "multipart"}
	files := []TransferTreeSessionFile{
		{RelativePath: "a.bin", Size: 10, ModTimeUnixNano: 100},
		{RelativePath: filepath.Join("nested", "b.bin"), Size: 20, ModTimeUnixNano: 200},
	}
	dirs := []TransferTreeSessionDirectory{{RelativePath: ""}, {RelativePath: "nested"}}
	session, err := OpenTransferTreeSession(path, spec, dirs, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MarkFileCompleted("a.bin"); err != nil {
		t.Fatal(err)
	}
	if err := session.SetFileSHA1("a.bin", "ABC"); err != nil {
		t.Fatal(err)
	}
	if err := session.SetDirectoryRemoteID("nested", "42"); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenTransferTreeSession(path, spec, dirs, files)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := reopened.Snapshot()
	if !snapshot.Files[0].Completed || snapshot.Files[0].SHA1 != "ABC" {
		t.Fatalf("completed state was not preserved: %#v", snapshot.Files)
	}
	if snapshot.Directories[1].RemoteID != "42" {
		t.Fatalf("directory ID was not preserved: %#v", snapshot.Directories)
	}

	changed := append([]TransferTreeSessionFile(nil), files...)
	changed[0].ModTimeUnixNano++
	changed = append(changed, TransferTreeSessionFile{RelativePath: "new.bin", Size: 1, ModTimeUnixNano: 1})
	reconciled, err := OpenTransferTreeSession(path, spec, dirs, changed)
	if err != nil {
		t.Fatal(err)
	}
	snapshot = reconciled.Snapshot()
	if snapshot.Files[0].Completed || snapshot.Files[0].SHA1 != "" {
		t.Fatalf("changed file incorrectly retained completion: %#v", snapshot.Files[0])
	}
	if len(snapshot.Files) != 3 {
		t.Fatalf("new file not reconciled: %#v", snapshot.Files)
	}
}

func TestTransferTreeSessionDownloadIdentityChangeResetsCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	spec := TransferTreeSessionSpec{Direction: "download", Source: "/remote", Destination: "C:/dst", Strategy: "file"}
	files := []TransferTreeSessionFile{{RelativePath: "a.bin", Size: 10, StableID: "id", PickCode: "pick", SHA1: "AAA"}}
	session, err := OpenTransferTreeSession(path, spec, nil, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.MarkFileCompleted("a.bin"); err != nil {
		t.Fatal(err)
	}
	files[0].SHA1 = "BBB"
	reopened, err := OpenTransferTreeSession(path, spec, nil, files)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Files[0].Completed {
		t.Fatal("changed remote SHA1 retained completion")
	}
}

func TestTransferTreeSessionWindowsAcceptsLegacyPathCasing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path identity test")
	}
	path := filepath.Join(t.TempDir(), "session.json")
	legacy := TransferTreeSessionSpec{Direction: "upload", Source: `C:\\Data\\Tree`, Destination: "/remote", Strategy: "multipart"}
	files := []TransferTreeSessionFile{{RelativePath: "a.bin", Size: 1, Completed: true}}
	now := time.Now().UTC()
	legacySnapshot := TransferTreeSessionSnapshot{
		Version: TransferTreeSessionVersion,
		KeyHash: transferTreeSessionKeyHash(legacy),
		Spec:    legacy, CreatedAt: now, UpdatedAt: now,
		Files: files,
	}
	encoded, err := json.MarshalIndent(legacySnapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	current := legacy
	current.Source = `c:\\data\\tree`
	reopened, err := OpenTransferTreeSession(path, current, nil, files)
	if err != nil {
		t.Fatalf("legacy path casing should remain resumable: %v", err)
	}
	if !reopened.Snapshot().Files[0].Completed {
		t.Fatal("legacy session completion was not preserved")
	}
}

func TestValidateTransferTreeSessionIsNonDestructive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-tree.session.json")
	spec := TransferTreeSessionSpec{Direction: "download", Source: "/remote", Destination: filepath.Join(t.TempDir(), "dst"), Strategy: "file"}
	if _, err := OpenTransferTreeSession(path, spec, nil, []TransferTreeSessionFile{{RelativePath: "a.bin", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	valid, err := ValidateTransferTreeSession(path, spec)
	if err != nil || !valid {
		t.Fatalf("valid legacy tree state was rejected: valid=%v err=%v", valid, err)
	}
	other := spec
	other.Source = "/other"
	valid, err = ValidateTransferTreeSession(path, other)
	if err != nil || valid {
		t.Fatalf("mismatched legacy tree state was accepted: valid=%v err=%v", valid, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("tree validator mutated legacy state: %v", err)
	}
}

func TestTransferTreeSessionRejectsDifferentTransferAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	spec := TransferTreeSessionSpec{Direction: "download", Source: "/a", Destination: "dst", Strategy: "file"}
	if _, err := OpenTransferTreeSession(path, spec, nil, []TransferTreeSessionFile{{RelativePath: "a", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	other := spec
	other.Source = "/b"
	if _, err := OpenTransferTreeSession(path, other, nil, []TransferTreeSessionFile{{RelativePath: "a", Size: 1}}); !errors.Is(err, ErrTransferTreeSession) {
		t.Fatalf("expected transfer mismatch, got %v", err)
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := OpenTransferTreeSession(link, spec, nil, []TransferTreeSessionFile{{RelativePath: "a", Size: 1}}); !errors.Is(err, ErrTransferTreeSession) {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}
