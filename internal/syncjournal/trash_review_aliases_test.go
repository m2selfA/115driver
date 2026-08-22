package syncjournal

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTrashReviewAliasesRoundTripAndLegacyMissing(t *testing.T) {
	trashDir := t.TempDir()
	ids := []string{
		"sha256:" + strings.Repeat("c", 64),
		"sha256:" + strings.Repeat("b", 64),
	}
	if err := WriteTrashReviewAliases(trashDir, ids); err != nil {
		t.Fatal(err)
	}
	got, found, err := ReadTrashReviewAliases(trashDir)
	if err != nil || !found || len(got) != 2 || got[0] != ids[1] || got[1] != ids[0] {
		t.Fatalf("trash review aliases=%#v found=%v err=%v", got, found, err)
	}
	if info, err := os.Stat(filepath.Join(trashDir, trashReviewAliasesFile)); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("trash review alias sidecar is not private: %v", info.Mode().Perm())
	}

	legacyDir := t.TempDir()
	got, found, err = ReadTrashReviewAliases(legacyDir)
	if err != nil || found || got != nil {
		t.Fatalf("legacy missing sidecar=%#v found=%v err=%v", got, found, err)
	}
}

func TestTrashReviewAliasesRejectsMalformedCanonicalState(t *testing.T) {
	trashDir := t.TempDir()
	path := filepath.Join(trashDir, trashReviewAliasesFile)
	bad := `{"version":1,"schema":"115driver.sync-journal-trash-review-aliases/v1","reviewed_plan_ids":["sha256:` + strings.Repeat("c", 64) + `","sha256:` + strings.Repeat("b", 64) + `"]}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ReadTrashReviewAliases(trashDir); !found || !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("non-canonical sidecar found=%v err=%v", found, err)
	}

	bad = `{"version":1,"schema":"115driver.sync-journal-trash-review-aliases/v1","reviewed_plan_ids":["sha256:` + strings.Repeat("b", 64) + `"],"extra":true}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ReadTrashReviewAliases(trashDir); !found || !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("unknown-field sidecar found=%v err=%v", found, err)
	}
}

func TestWriteTrashReviewAliasesRejectsDuplicatesAndSymlinkDir(t *testing.T) {
	trashDir := t.TempDir()
	id := "sha256:" + strings.Repeat("b", 64)
	if err := WriteTrashReviewAliases(trashDir, []string{id, id}); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("duplicate review IDs error=%v", err)
	}

	if err := os.Symlink(trashDir, filepath.Join(t.TempDir(), "trash-link")); err == nil {
		link := filepath.Join(filepath.Dir(filepath.Join(t.TempDir(), "unused")), "missing")
		_ = link
	}
}
