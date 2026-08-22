package cmd

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestNormalizeLSPageDefaultsToBoundedLimit(t *testing.T) {
	offset, limit := normalizeLSPage(0, 0)
	if offset != 0 {
		t.Fatalf("unexpected offset: %d", offset)
	}
	if limit != defaultLSLimit {
		t.Fatalf("expected default limit %d, got %d", defaultLSLimit, limit)
	}
}

func TestNormalizeLSPageCapsLargeLimit(t *testing.T) {
	_, limit := normalizeLSPage(0, maxLSLimit+1)
	if limit != maxLSLimit {
		t.Fatalf("expected max limit %d, got %d", maxLSLimit, limit)
	}
}

func TestBuildLSJSONResponseIncludesPaginationMetadata(t *testing.T) {
	resp := buildLSJSONResponse("/", nil, 10, 5)
	if resp["offset"] != int64(10) {
		t.Fatalf("unexpected offset: %v", resp["offset"])
	}
	if resp["limit"] != int64(5) {
		t.Fatalf("unexpected limit: %v", resp["limit"])
	}
	if resp["has_more"] != false {
		t.Fatalf("unexpected has_more: %v", resp["has_more"])
	}
}

func TestBuildLSTextPaginationNoticeShowsNextOffsetWhenPageIsFull(t *testing.T) {
	got := buildLSTextPaginationNotice(100, 200, 100)
	want := "Showing 100 entries. Use --offset 300 to continue.\n"
	if got != want {
		t.Fatalf("unexpected notice: got %q want %q", got, want)
	}
}

func TestBuildLSTextPaginationNoticeEmptyWhenPageIsNotFull(t *testing.T) {
	if got := buildLSTextPaginationNotice(99, 0, 100); got != "" {
		t.Fatalf("expected no notice, got %q", got)
	}
}

func TestLoadLSListingPreservesSinglePathPaginationShape(t *testing.T) {
	oldRecursive, oldOffset, oldLimit, oldDepth := lsRecursive, lsOffset, lsLimit, lsMaxDepth
	t.Cleanup(func() {
		lsRecursive, lsOffset, lsLimit, lsMaxDepth = oldRecursive, oldOffset, oldLimit, oldDepth
	})
	lsRecursive = false
	lsOffset = 0
	lsLimit = 2
	lsMaxDepth = 0
	client := &fakeMetadataClient{
		dirIDs: map[string]string{"folder": "d1"},
		lists: map[string][]driver.File{
			"d1": {
				{FileID: "f1", Name: "a.bin", Size: 1},
				{FileID: "f2", Name: "sub", IsDirectory: true},
				{FileID: "f3", Name: "later.bin", Size: 3},
			},
		},
	}
	result, err := loadLSListing(client, resolver.New(client), "/folder")
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "/folder" || result.Recursive || len(result.Files) != 2 || !result.HasMore || result.Offset != 0 || result.Limit != 2 {
		t.Fatalf("unexpected ls listing: %#v", result)
	}
	if data := result.jsonData(); data["path"] != "/folder" || data["next_offset"] != int64(2) || data["has_more"] != true {
		t.Fatalf("unexpected ls JSON data: %#v", data)
	}
}

func TestRunLSBatchContinueOnErrorProcessesLaterPaths(t *testing.T) {
	oldRecursive, oldOffset, oldLimit, oldDepth := lsRecursive, lsOffset, lsLimit, lsMaxDepth
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		lsRecursive, lsOffset, lsLimit, lsMaxDepth = oldRecursive, oldOffset, oldLimit, oldDepth
		jsonOutput, printer = oldJSON, oldPrinter
	})
	lsRecursive = false
	lsOffset = 0
	lsLimit = 100
	lsMaxDepth = 0
	jsonOutput = true
	printer = output.NewPrinter(false)

	cmd := newBatchInputTestCommand(t, "")
	if err := cmd.Flags().Set("continue-on-error", "true"); err != nil {
		t.Fatal(err)
	}
	client := &fakeMetadataClient{
		dirIDs: map[string]string{"folder": "d1"},
		lists: map[string][]driver.File{
			"d1": {{FileID: "f1", Name: "a.bin", Size: 1}},
		},
	}
	err := runLSCommand(client, cmd, []string{"/missing", "/folder"})
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected batch exitError, got %T %v", err, err)
	}
	data, ok := ee.data.(map[string]interface{})
	if !ok {
		t.Fatalf("batch error data type = %T", ee.data)
	}
	if data["processed"] != 2 || data["failed"] != 1 || data["succeeded"] != 1 || data["remaining"] != 0 {
		t.Fatalf("unexpected batch accounting: %#v", data)
	}
	entries, ok := data["entries"].([]lsBatchResult)
	if !ok || len(entries) != 1 || entries[0].Path != "/folder" {
		t.Fatalf("later successful path was not preserved: %#v", data["entries"])
	}
}
