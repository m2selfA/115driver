package resolver

import (
	"errors"
	"net/url"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestResolvePath_FallsBackToFileWhenDirLookupReturnsRootID(t *testing.T) {
	client := fakeResolverClient{
		dirIDs: map[string]string{
			"q9tVD1jYR8e626EteJ0qDQ.mp4": RootID,
		},
		pagesByDir: map[string][][]driver.File{
			RootID: {
				{
					{
						FileID:      "123456",
						Name:        "q9tVD1jYR8e626EteJ0qDQ.mp4",
						IsDirectory: false,
					},
				},
			},
		},
	}

	fileID, isDir, err := ResolvePath(&client, "q9tVD1jYR8e626EteJ0qDQ.mp4")
	if err != nil {
		t.Fatalf("ResolvePath returned error: %v", err)
	}
	if isDir {
		t.Fatalf("ResolvePath should treat file path as file")
	}
	if fileID != "123456" {
		t.Fatalf("unexpected file ID: %s", fileID)
	}
}

func TestResolvePathDoesNotFallBackAfterDirectoryNetworkFailure(t *testing.T) {
	networkErr := &url.Error{Op: "Get", URL: "https://example.invalid", Err: errors.New("offline")}
	client := &fakeResolverClient{dirErrs: map[string]error{"movies": networkErr}}

	_, _, err := ResolvePath(client, "movies")
	var gotURL *url.Error
	if !errors.As(err, &gotURL) {
		t.Fatalf("ResolvePath network error = %T %v, want url.Error", err, err)
	}
	if client.listPageCalls != 0 {
		t.Fatalf("network failure triggered %d file-list fallback calls, want 0", client.listPageCalls)
	}
}

func TestResolvePathMissingCarriesNotExistSentinel(t *testing.T) {
	_, _, err := ResolvePath(&fakeResolverClient{}, "missing.bin")
	if !errors.Is(err, driver.ErrNotExist) {
		t.Fatalf("ResolvePath missing error = %v, want ErrNotExist", err)
	}
}

func TestResolvePath_RootStillResolvesToDirectory(t *testing.T) {
	fileID, isDir, err := ResolvePath(&fakeResolverClient{}, "/")
	if err != nil {
		t.Fatalf("ResolvePath returned error: %v", err)
	}
	if !isDir {
		t.Fatalf("root path should resolve as directory")
	}
	if fileID != RootID {
		t.Fatalf("unexpected root ID: %s", fileID)
	}
}

func TestPathResolverCachesSuccessfulDirectoryLookups(t *testing.T) {
	client := &fakeResolverClient{dirIDs: map[string]string{"movies": "dir1"}}
	resolver := New(client)

	for _, remotePath := range []string{"/movies/", "movies"} {
		id, err := resolver.ResolveDir(remotePath)
		if err != nil {
			t.Fatalf("ResolveDir(%q) returned error: %v", remotePath, err)
		}
		if id != "dir1" {
			t.Fatalf("ResolveDir(%q) = %q, want dir1", remotePath, id)
		}
	}
	if got := client.dirCalls["movies"]; got != 1 {
		t.Fatalf("successful directory lookup calls = %d, want 1", got)
	}
}

func TestPathResolverCacheIsScopedToResolverInstance(t *testing.T) {
	firstClient := &fakeResolverClient{dirIDs: map[string]string{"movies": "account-a"}}
	secondClient := &fakeResolverClient{dirIDs: map[string]string{"movies": "account-b"}}

	firstID, err := New(firstClient).ResolveDir("movies")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := New(secondClient).ResolveDir("movies")
	if err != nil {
		t.Fatal(err)
	}
	if firstID != "account-a" || secondID != "account-b" {
		t.Fatalf("resolver instances leaked identities: first=%q second=%q", firstID, secondID)
	}
	if firstClient.dirCalls["movies"] != 1 || secondClient.dirCalls["movies"] != 1 {
		t.Fatalf("each scoped resolver should query its own client once: first=%d second=%d", firstClient.dirCalls["movies"], secondClient.dirCalls["movies"])
	}
}

func TestPathResolverLRUEvictsLeastRecentlyUsedDirectory(t *testing.T) {
	client := &fakeResolverClient{dirIDs: map[string]string{"a": "1", "b": "2", "c": "3"}}
	resolver := newPathResolver(client, 2)

	for _, remotePath := range []string{"a", "b", "a", "c", "b"} {
		if _, err := resolver.ResolveDir(remotePath); err != nil {
			t.Fatalf("ResolveDir(%q) returned error: %v", remotePath, err)
		}
	}
	if got := client.dirCalls["a"]; got != 1 {
		t.Fatalf("lookup calls for a = %d, want 1", got)
	}
	if got := client.dirCalls["b"]; got != 2 {
		t.Fatalf("lookup calls for evicted b = %d, want 2", got)
	}
	if got := client.dirCalls["c"]; got != 1 {
		t.Fatalf("lookup calls for c = %d, want 1", got)
	}
}

func TestPathResolverDoesNotCacheMissingDirectory(t *testing.T) {
	client := &fakeResolverClient{}
	resolver := New(client)
	for i := 0; i < 2; i++ {
		if _, err := resolver.ResolveDir("missing"); err == nil {
			t.Fatal("expected missing directory error")
		}
	}
	if got := client.dirCalls["missing"]; got != 2 {
		t.Fatalf("missing directory lookup calls = %d, want 2", got)
	}
}

func TestResolveDirCompatibilityWrapperDoesNotShareCache(t *testing.T) {
	client := &fakeResolverClient{dirIDs: map[string]string{"movies": "dir1"}}
	for i := 0; i < 2; i++ {
		if _, err := ResolveDir(client, "movies"); err != nil {
			t.Fatalf("ResolveDir returned error: %v", err)
		}
	}
	if got := client.dirCalls["movies"]; got != 2 {
		t.Fatalf("compatibility wrapper lookup calls = %d, want 2", got)
	}
}

func TestResolveFileSearchesDirectoryByPages(t *testing.T) {
	client := fakeResolverClient{
		dirIDs: map[string]string{"movies": "dir1"},
		pagesByDir: map[string][][]driver.File{
			"dir1": {
				repeatFiles(fileResolvePageLimit, "filler"),
				{{FileID: "2", Name: "target.mp4"}},
			},
		},
	}

	fileID, err := ResolveFile(&client, "/movies/target.mp4")
	if err != nil {
		t.Fatalf("ResolveFile returned error: %v", err)
	}
	if fileID != "2" {
		t.Fatalf("unexpected file ID: %s", fileID)
	}
	if client.listAllCalls != 0 {
		t.Fatalf("expected ResolveFile not to call full List, got %d calls", client.listAllCalls)
	}
	if client.listPageCalls != 2 {
		t.Fatalf("expected ResolveFile to scan 2 pages, got %d", client.listPageCalls)
	}
}

type fakeResolverClient struct {
	dirIDs        map[string]string
	dirErrs       map[string]error
	filesByDir    map[string][]driver.File
	pagesByDir    map[string][][]driver.File
	listAllCalls  int
	listPageCalls int
	dirCalls      map[string]int
}

func (f *fakeResolverClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	if f.dirCalls == nil {
		f.dirCalls = make(map[string]int)
	}
	f.dirCalls[dir]++
	if err := f.dirErrs[dir]; err != nil {
		return nil, err
	}
	id := f.dirIDs[dir]
	return &driver.APIGetDirIDResp{
		CategoryID: driver.IntString(id),
	}, nil
}

func (f *fakeResolverClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	f.listAllCalls++
	files := f.filesByDir[dirID]
	return &files, nil
}

func (f *fakeResolverClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
	f.listPageCalls++
	page := int(offset / limit)
	pages := f.pagesByDir[dirID]
	if page >= len(pages) {
		files := []driver.File{}
		return &files, nil
	}
	files := pages[page]
	return &files, nil
}

func repeatFiles(count int64, prefix string) []driver.File {
	files := make([]driver.File, 0, count)
	for i := int64(0); i < count; i++ {
		files = append(files, driver.File{FileID: prefix, Name: prefix})
	}
	return files
}
