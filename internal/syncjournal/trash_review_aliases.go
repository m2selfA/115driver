package syncjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const (
	trashReviewAliasesVersion = 1
	trashReviewAliasesSchema  = "115driver.sync-journal-trash-review-aliases/v1"
	trashReviewAliasesFile    = "review-aliases.json"
)

type trashReviewAliasesRecord struct {
	Version         int      `json:"version"`
	Schema          string   `json:"schema"`
	ReviewedPlanIDs []string `json:"reviewed_plan_ids"`
}

func canonicalTrashReviewAliases(reviewIDs []string) ([]string, error) {
	if len(reviewIDs) == 0 {
		return nil, errors.New("trashed sync journal review alias set is empty")
	}
	canonical := make([]string, 0, len(reviewIDs))
	seen := make(map[string]struct{}, len(reviewIDs))
	for _, reviewID := range reviewIDs {
		normalized, err := NormalizeReviewID(reviewID)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized]; exists {
			return nil, fmt.Errorf("%w: duplicate trashed sync journal review alias", ErrInvalidSchema)
		}
		seen[normalized] = struct{}{}
		canonical = append(canonical, normalized)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func validateTrashReviewAliasesDir(trashDir string) (string, error) {
	trashDir = strings.TrimSpace(trashDir)
	if trashDir == "" {
		return "", errors.New("trashed sync journal directory is empty")
	}
	absolute, err := filepath.Abs(trashDir)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("trashed sync journal path is not a real directory")
	}
	return absolute, nil
}

// WriteTrashReviewAliases persists the complete reviewed-ID set removed when a
// current journal is moved into shared Session Store trash. The sidecar lives
// inside the trash directory, so normal trash_retention purges it together with
// the journal and no new global metadata lifecycle is introduced.
func WriteTrashReviewAliases(trashDir string, reviewIDs []string) error {
	absolute, err := validateTrashReviewAliasesDir(trashDir)
	if err != nil {
		return err
	}
	canonical, err := canonicalTrashReviewAliases(reviewIDs)
	if err != nil {
		return err
	}
	record := trashReviewAliasesRecord{
		Version: trashReviewAliasesVersion, Schema: trashReviewAliasesSchema,
		ReviewedPlanIDs: canonical,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := transfer.WritePrivateFileAtomic(filepath.Join(absolute, trashReviewAliasesFile), encoded); err != nil {
		return fmt.Errorf("write trashed sync journal review aliases: %w", err)
	}
	return nil
}

// ReadTrashReviewAliases loads the optional multi-alias sidecar. found=false is
// the backward-compatible signal for trash entries created before this sidecar
// existed; reviewed restore callers may then use their explicitly supplied
// reviewed plan ID, while raw CLI restore has no alias set to recreate.
func ReadTrashReviewAliases(trashDir string) (reviewIDs []string, found bool, err error) {
	absolute, err := validateTrashReviewAliasesDir(trashDir)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(absolute, trashReviewAliasesFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var record trashReviewAliasesRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, true, fmt.Errorf("%w: decode trashed sync journal review aliases: %v", ErrInvalidSchema, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, true, fmt.Errorf("%w: trashed sync journal review aliases contain trailing data", ErrInvalidSchema)
	}
	if record.Version != trashReviewAliasesVersion || record.Schema != trashReviewAliasesSchema {
		return nil, true, fmt.Errorf("%w: unsupported trashed sync journal review alias schema", ErrInvalidSchema)
	}
	canonical, err := canonicalTrashReviewAliases(record.ReviewedPlanIDs)
	if err != nil {
		return nil, true, err
	}
	if len(canonical) != len(record.ReviewedPlanIDs) {
		return nil, true, fmt.Errorf("%w: invalid trashed sync journal review alias set", ErrInvalidSchema)
	}
	for index := range canonical {
		if canonical[index] != record.ReviewedPlanIDs[index] {
			return nil, true, fmt.Errorf("%w: non-canonical trashed sync journal review alias ordering", ErrInvalidSchema)
		}
	}
	return canonical, true, nil
}
