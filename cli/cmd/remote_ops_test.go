package cmd

import (
	"reflect"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeRemotePathClient struct {
	dirIDs    map[string]string
	dirErrs   map[string]error
	lists     map[string][]driver.File
	listErrs  map[string]error
	nilLists  map[string]bool
	dirCalls  map[string]int
	listCalls int
}

func (client *fakeRemotePathClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	if client.dirCalls == nil {
		client.dirCalls = make(map[string]int)
	}
	client.dirCalls[dir]++
	if err := client.dirErrs[dir]; err != nil {
		return nil, err
	}
	id, ok := client.dirIDs[dir]
	if !ok {
		return nil, driver.ErrNotExist
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(id)}, nil
}

func (client *fakeRemotePathClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	client.listCalls++
	if err := client.listErrs[dirID]; err != nil {
		return nil, err
	}
	if client.nilLists[dirID] {
		return nil, nil
	}
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *fakeRemotePathClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
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

func testRemotePathClient() *fakeRemotePathClient {
	return &fakeRemotePathClient{
		dirIDs: map[string]string{"folder": "d-folder", "dest": "d-dest"},
		lists: map[string][]driver.File{
			"0": {{FileID: "f-a", Name: "a.txt", Size: 1}},
		},
	}
}

func TestWalkRemoteTreeRejectsNilListingAndUnsafeNamesBeforeVisiting(t *testing.T) {
	for name, client := range map[string]*fakeRemotePathClient{
		"nil-list":             {nilLists: map[string]bool{"root": true}},
		"unsafe-name":          {lists: map[string][]driver.File{"root": {{FileID: "f1", Name: "../escape.bin"}}}},
		"directory-without-id": {lists: map[string][]driver.File{"root": {{Name: "sub", IsDirectory: true}}}},
	} {
		t.Run(name, func(t *testing.T) {
			visits := 0
			_, err := walkRemoteTree(client, "root", "/root", 0, func(remoteWalkEntry) (bool, error) {
				visits++
				return false, nil
			})
			if err == nil {
				t.Fatal("expected malformed remote tree error")
			}
			if visits != 0 {
				t.Fatalf("malformed entry reached visitor %d time(s)", visits)
			}
		})
	}
}

func TestResolveUniqueRemoteItemsHandlesMixedFilesDirectoriesAndDuplicates(t *testing.T) {
	items, err := resolveUniqueRemoteItems(testRemotePathClient(), []string{"/a.txt", "/folder", "/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected duplicate file ID to be removed: %#v", items)
	}
	if items[0].ID != "f-a" || items[0].IsDir || items[1].ID != "d-folder" || !items[1].IsDir {
		t.Fatalf("unexpected resolved items: %#v", items)
	}
}

func TestMoveOrCopySubmitsResolvedSourcesInOneBatch(t *testing.T) {
	oldPrinter, oldJSON := printer, jsonOutput
	printer = output.NewPrinter(false)
	jsonOutput = true
	defer func() { printer, jsonOutput = oldPrinter, oldJSON }()

	var gotDir string
	var gotIDs []string
	err := moveOrCopy("copy", testRemotePathClient(), []string{"/a.txt", "/folder", "/a.txt"}, "/dest", func(dirID string, fileIDs ...string) error {
		gotDir = dirID
		gotIDs = append([]string(nil), fileIDs...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != "d-dest" || !reflect.DeepEqual(gotIDs, []string{"f-a", "d-folder"}) {
		t.Fatalf("unexpected batch transfer: dir=%q ids=%v", gotDir, gotIDs)
	}
}

func TestMoveOrCopyRejectsRemoteRootSource(t *testing.T) {
	called := false
	err := moveOrCopy("move", testRemotePathClient(), []string{"/"}, "/dest", func(string, ...string) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected remote root source to be rejected")
	}
	if called {
		t.Fatal("root mutation reached the transfer callback")
	}
}

func TestMoveOrCopyDoesNotSubmitWhenAnySourceIsMissing(t *testing.T) {
	called := false
	err := moveOrCopy("move", testRemotePathClient(), []string{"/a.txt", "/missing.txt"}, "/dest", func(string, ...string) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected missing source to fail")
	}
	if called {
		t.Fatal("batch operation was submitted before all sources resolved")
	}
}
