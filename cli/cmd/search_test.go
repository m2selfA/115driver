package cmd

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func preserveSearchFlags(t *testing.T) {
	t.Helper()
	oldType, oldSort, oldLimit := searchType, searchSort, searchLimit
	oldOffset, oldDir, oldAsc := searchOffset, searchDir, searchAsc
	t.Cleanup(func() {
		searchType, searchSort, searchLimit = oldType, oldSort, oldLimit
		searchOffset, searchDir, searchAsc = oldOffset, oldDir, oldAsc
	})
}

func TestSearchArgsRejectsInvalidPaginationAndType(t *testing.T) {
	preserveSearchFlags(t)
	searchLimit = 30
	searchOffset = 0
	searchType = ""

	for name, configure := range map[string]func(){
		"limit":  func() { searchLimit = 0 },
		"offset": func() { searchOffset = -1 },
		"type":   func() { searchType = "made-up-type" },
	} {
		t.Run(name, func(t *testing.T) {
			preserveSearchFlags(t)
			searchLimit = 30
			searchOffset = 0
			searchType = ""
			configure()
			err := searchArgs(searchCmd, []string{"needle"})
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("searchArgs error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestBuildSearchOptionsPreservesDescendingDefaultAndSupportsAscending(t *testing.T) {
	preserveSearchFlags(t)
	searchType = "video"
	searchSort = "file_size"
	searchLimit = 20
	searchOffset = 40
	searchAsc = false

	opts := buildSearchOptions("needle", "cid-1")
	if opts.SearchValue != "needle" || opts.Cid != "cid-1" || opts.Type != 4 || opts.Order != "file_size" || opts.Limit != 20 || opts.Offset != 40 || opts.Asc != 0 {
		t.Fatalf("unexpected default search options: %#v", opts)
	}

	searchAsc = true
	if opts := buildSearchOptions("needle", "cid-1"); opts.Asc != 1 {
		t.Fatalf("ascending search options asc = %d, want 1", opts.Asc)
	}
}

func TestBuildSearchJSONResponseIncludesPaginationMetadata(t *testing.T) {
	opts := &driver.SearchOption{Offset: 10, Limit: 2, Asc: 0}
	result := &driver.SearchResult{Count: 15, Order: "file_name"}
	files := []output.JSONFile{{Name: "a"}, {Name: "b"}}
	response := buildSearchJSONResponse("needle", "/dir", opts, result, files)

	if response["offset"] != 10 || response["limit"] != 2 || response["next_offset"] != 12 || response["has_more"] != true || response["ascending"] != false || response["order"] != "file_name" {
		t.Fatalf("unexpected search response metadata: %#v", response)
	}
}

func TestWhoamiRejectsPositionalArguments(t *testing.T) {
	if err := whoamiCmd.Args(whoamiCmd, []string{"unexpected"}); err == nil {
		t.Fatal("whoami accepted an unexpected positional argument")
	}
}
