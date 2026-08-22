package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeMetadataClient struct {
	dirIDs map[string]string
	lists  map[string][]driver.File
	files  map[string]driver.File
	stats  map[string]driver.FileStatInfo
}

func (client *fakeMetadataClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	id, ok := client.dirIDs[dir]
	if !ok {
		return nil, driver.ErrNotExist
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(id)}, nil
}

func (client *fakeMetadataClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *fakeMetadataClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
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

func (client *fakeMetadataClient) Stat(fileID string) (*driver.FileStatInfo, error) {
	stat, ok := client.stats[fileID]
	if !ok {
		return nil, errors.New("stat missing")
	}
	clone := stat
	return &clone, nil
}

func (client *fakeMetadataClient) GetFile(fileID string) (*driver.File, error) {
	file, ok := client.files[fileID]
	if !ok {
		return nil, errors.New("file missing")
	}
	clone := file
	return &clone, nil
}

func TestLoadRemoteStatHandlesFileAndDirectory(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	client := &fakeMetadataClient{
		dirIDs: map[string]string{"folder": "d1"},
		lists: map[string][]driver.File{
			"0": {{FileID: "f1", Name: "a.bin", Size: 7}},
		},
		files: map[string]driver.File{"f1": {FileID: "f1", Name: "a.bin", Size: 7}},
		stats: map[string]driver.FileStatInfo{
			"f1": {Name: "a.bin", Sha1: "ABC", PickCode: "pick", CreateTime: now, UpdateTime: now},
			"d1": {Name: "folder", IsDirectory: true, FileCount: 3, DirCount: 2, CreateTime: now, UpdateTime: now},
		},
	}

	fileStat, err := loadRemoteStat(client, "/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if fileStat.Name != "a.bin" || fileStat.IsDir || fileStat.Size != 7 || fileStat.Sha1 != "ABC" || fileStat.PickCode != "pick" {
		t.Fatalf("unexpected file stat: %#v", fileStat)
	}

	dirStat, err := loadRemoteStat(client, "/folder")
	if err != nil {
		t.Fatal(err)
	}
	if !dirStat.IsDir || dirStat.FileCount != 3 || dirStat.DirCount != 2 {
		t.Fatalf("unexpected directory stat: %#v", dirStat)
	}
}

func TestSummarizeRemoteUsageHandlesMultipleKinds(t *testing.T) {
	client := &fakeMetadataClient{
		dirIDs: map[string]string{"folder": "d1"},
		lists: map[string][]driver.File{
			"0":  {{FileID: "f1", Name: "a.bin", Size: 7}},
			"d1": {{FileID: "f2", Name: "child.bin", Size: 5}},
		},
		files: map[string]driver.File{"f1": {FileID: "f1", Name: "a.bin", Size: 7}},
	}

	fileSummary, err := summarizeRemoteUsage(client, "/a.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fileSummary.Size != 7 || fileSummary.Files != 1 || fileSummary.Directories != 0 || !fileSummary.Complete {
		t.Fatalf("unexpected file usage: %#v", fileSummary)
	}

	dirSummary, err := summarizeRemoteUsage(client, "/folder", 0)
	if err != nil {
		t.Fatal(err)
	}
	if dirSummary.Size != 5 || dirSummary.Files != 1 || dirSummary.Directories != 0 || !dirSummary.Complete {
		t.Fatalf("unexpected directory usage: %#v", dirSummary)
	}
}
