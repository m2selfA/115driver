package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReviewAliasScan is a bounded, path-free snapshot of validated private review
// aliases for one profile/account scope. ByPlanID supports reverse lookup from
// the internal journal fingerprint without exposing the alias filesystem
// layout to callers.
type ReviewAliasScan struct {
	Aliases  []ReviewAlias
	ByPlanID map[string][]string
}

// ScanReviewAliases reads and validates at most maxAliases review-alias JSON
// files for this exact profile/account. Any malformed/non-canonical alias fails
// the whole scan instead of returning a partial reverse index.
func (store Store) ScanReviewAliases(maxAliases int) (ReviewAliasScan, error) {
	return store.scanReviewAliases(maxAliases, true)
}

// ScanReviewAliasesProfile is the offline-admin companion: it validates the
// exact profile scope and each alias's persisted positive account binding, but
// does not require the caller to know that account in advance. It is intended
// for local doctor/maintenance only; authenticated MCP paths use ScanReviewAliases.
func (store Store) ScanReviewAliasesProfile(maxAliases int) (ReviewAliasScan, error) {
	return store.scanReviewAliases(maxAliases, false)
}

func (store Store) scanReviewAliases(maxAliases int, requireAccountMatch bool) (ReviewAliasScan, error) {
	if maxAliases <= 0 {
		return ReviewAliasScan{}, errors.New("sync journal review alias scan limit must be > 0")
	}
	root, err := store.RootPath()
	if err != nil {
		return ReviewAliasScan{}, err
	}
	aliasRoot := filepath.Join(root, "review-aliases")
	scan := ReviewAliasScan{
		Aliases:  make([]ReviewAlias, 0),
		ByPlanID: make(map[string][]string),
	}
	seen := 0
	err = filepath.WalkDir(aliasRoot, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		seen++
		if seen > maxAliases {
			return fmt.Errorf("%w: maximum %d review alias files", ErrScanLimit, maxAliases)
		}
		raw := strings.TrimSuffix(entry.Name(), ".json")
		planID, normalizeErr := NormalizePlanID(raw)
		if normalizeErr != nil || planID != raw {
			return fmt.Errorf("%w: non-canonical sync journal review alias filename", ErrInvalidSchema)
		}
		reviewID := "sha256:" + raw
		expectedPath, pathErr := store.reviewAliasPath(reviewID)
		if pathErr != nil {
			return pathErr
		}
		if filepath.Clean(current) != filepath.Clean(expectedPath) {
			return fmt.Errorf("%w: non-canonical sync journal review alias layout", ErrInvalidSchema)
		}
		var alias ReviewAlias
		var readErr error
		if requireAccountMatch {
			alias, readErr = store.readReviewAlias(reviewID)
		} else {
			alias, readErr = store.readReviewAliasProfileBound(reviewID)
		}
		if readErr != nil {
			return fmt.Errorf("inspect sync journal review alias: %w", readErr)
		}
		scan.Aliases = append(scan.Aliases, alias)
		scan.ByPlanID[alias.PlanID] = append(scan.ByPlanID[alias.PlanID], alias.ReviewID)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return ReviewAliasScan{}, err
	}
	sort.Slice(scan.Aliases, func(i, j int) bool {
		if scan.Aliases[i].UpdatedAt.Equal(scan.Aliases[j].UpdatedAt) {
			return scan.Aliases[i].ReviewID < scan.Aliases[j].ReviewID
		}
		return scan.Aliases[i].UpdatedAt.After(scan.Aliases[j].UpdatedAt)
	})
	for planID := range scan.ByPlanID {
		sort.Strings(scan.ByPlanID[planID])
	}
	return scan, nil
}
