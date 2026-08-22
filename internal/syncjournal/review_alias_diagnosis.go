package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type ReviewAliasDiagnosisStatus string

const (
	ReviewAliasDiagnosisLive             ReviewAliasDiagnosisStatus = "live"
	ReviewAliasDiagnosisOrphan           ReviewAliasDiagnosisStatus = "orphan"
	ReviewAliasDiagnosisSoftDeleted      ReviewAliasDiagnosisStatus = "soft-deleted-shadow"
	ReviewAliasDiagnosisIdentityMismatch ReviewAliasDiagnosisStatus = "identity-mismatch"
	ReviewAliasDiagnosisInvalidTarget    ReviewAliasDiagnosisStatus = "invalid-target"
)

// ReviewAliasTargetVerifier lets a caller bind frontend-specific reviewed-plan
// identity into the shared lifecycle diagnosis. The sync-journal package owns
// storage/current/trash/account classification; for example MCP supplies its
// content-addressed plan envelope check here.
type ReviewAliasTargetVerifier func(alias ReviewAlias, journal Journal) (bool, error)

type ReviewAliasDiagnosis struct {
	Alias  ReviewAlias
	Status ReviewAliasDiagnosisStatus
	InUse  bool
	Err    error
}

type ReviewAliasDiagnosisScan struct {
	Scanned          int
	Live             int
	Orphan           int
	SoftDeleted      int
	IdentityMismatch int
	Invalid          int
	Issues           int
	Entries          []ReviewAliasDiagnosis
}

// DiagnoseReviewAliases diagnoses aliases for one exact profile/account scope.
// Missing current targets are checked against one bounded trash pass. Only
// trash entries whose raw plan ID could satisfy a missing alias are decoded;
// malformed matching evidence fails closed, while unrelated damaged trash does
// not prevent proving another alias orphaned.
func (store Store) DiagnoseReviewAliases(maxAliases, maxTrash int, verifier ReviewAliasTargetVerifier) (ReviewAliasDiagnosisScan, error) {
	return store.diagnoseReviewAliases(maxAliases, maxTrash, false, verifier)
}

// DiagnoseReviewAliasesProfile is the offline-admin variant. The caller does
// not need to know an account up front; each validated alias supplies its own
// positive persisted account binding for current/trash target checks.
func (store Store) DiagnoseReviewAliasesProfile(maxAliases, maxTrash int, verifier ReviewAliasTargetVerifier) (ReviewAliasDiagnosisScan, error) {
	return store.diagnoseReviewAliases(maxAliases, maxTrash, true, verifier)
}

func reviewAliasTrashKey(planID string, accountID int64) string {
	return fmt.Sprintf("%s:%d", planID, accountID)
}

func (store Store) scanReviewAliasTrashTargets(targets map[string]map[int64]struct{}, maxTrash int) (map[string]struct{}, error) {
	if maxTrash <= 0 {
		return nil, errors.New("sync journal trash scan limit must be > 0")
	}
	present := make(map[string]struct{})
	if len(targets) == 0 {
		return present, nil
	}
	root, err := store.trashRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return present, nil
	}
	if err != nil {
		return nil, err
	}
	profileStore := store
	profileStore.AccountID = 0
	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		planID, ok := parseSyncJournalTrashPlanID(entry.Name())
		if !ok {
			continue
		}
		seen++
		if seen > maxTrash {
			return nil, fmt.Errorf("%w: maximum %d journal directories", ErrTrashScanLimit, maxTrash)
		}
		accounts, relevant := targets[planID]
		if !relevant {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("%w: matching trashed sync journal is not a real directory", ErrInvalidSchema)
		}
		data, readErr := os.ReadFile(filepath.Join(path, "journal.json"))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return nil, fmt.Errorf("%w: matching trashed sync journal is missing journal.json", ErrInvalidSchema)
			}
			return nil, readErr
		}
		journal, decodeErr := DecodeCurrent(data)
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: inspect matching trashed sync journal: %v", ErrInvalidSchema, decodeErr)
		}
		if journal.PlanID != planID {
			return nil, fmt.Errorf("%w: matching trash name does not match journal plan ID", ErrInvalidSchema)
		}
		if bindingErr := profileStore.ValidateBinding(journal); bindingErr != nil {
			if errors.Is(bindingErr, ErrBindingMismatch) {
				// The shared trash namespace spans profile scopes. A valid entry
				// from another profile cannot satisfy this profile's alias.
				continue
			}
			return nil, bindingErr
		}
		if journal.AccountID <= 0 {
			return nil, fmt.Errorf("%w: matching trashed journal has no account binding", ErrBindingMismatch)
		}
		if _, wantedAccount := accounts[journal.AccountID]; wantedAccount {
			present[reviewAliasTrashKey(planID, journal.AccountID)] = struct{}{}
		}
	}
	return present, nil
}

func (store Store) diagnoseReviewAliases(maxAliases, maxTrash int, profileOnly bool, verifier ReviewAliasTargetVerifier) (ReviewAliasDiagnosisScan, error) {
	if maxTrash <= 0 {
		return ReviewAliasDiagnosisScan{}, errors.New("sync journal trash scan limit must be > 0")
	}
	var aliasScan ReviewAliasScan
	var err error
	if profileOnly {
		aliasScan, err = store.ScanReviewAliasesProfile(maxAliases)
	} else {
		aliasScan, err = store.ScanReviewAliases(maxAliases)
	}
	if err != nil {
		return ReviewAliasDiagnosisScan{}, err
	}

	result := ReviewAliasDiagnosisScan{
		Scanned: len(aliasScan.Aliases),
		Entries: make([]ReviewAliasDiagnosis, len(aliasScan.Aliases)),
	}
	missing := make([]int, 0)
	trashTargets := make(map[string]map[int64]struct{})
	for index, alias := range aliasScan.Aliases {
		entry := ReviewAliasDiagnosis{Alias: alias}
		aliasStore := store
		aliasStore.AccountID = alias.AccountID
		record, currentErr := aliasStore.InspectCurrentRecord(alias.PlanID)
		switch {
		case currentErr == nil:
			entry.InUse = record.InUse
			if record.Journal.AccountID <= 0 || record.Journal.AccountID != alias.AccountID {
				entry.Status = ReviewAliasDiagnosisInvalidTarget
				entry.Err = fmt.Errorf("%w: alias target journal account binding does not match", ErrBindingMismatch)
				result.Invalid++
				break
			}
			if verifier != nil {
				matches, verifyErr := verifier(alias, record.Journal)
				if verifyErr != nil {
					entry.Status = ReviewAliasDiagnosisInvalidTarget
					entry.Err = verifyErr
					result.Invalid++
					break
				}
				if !matches {
					entry.Status = ReviewAliasDiagnosisIdentityMismatch
					result.IdentityMismatch++
					break
				}
			}
			entry.Status = ReviewAliasDiagnosisLive
			result.Live++
		case errors.Is(currentErr, ErrNotFound):
			missing = append(missing, index)
			accounts := trashTargets[alias.PlanID]
			if accounts == nil {
				accounts = make(map[int64]struct{})
				trashTargets[alias.PlanID] = accounts
			}
			accounts[alias.AccountID] = struct{}{}
		default:
			entry.Status = ReviewAliasDiagnosisInvalidTarget
			entry.Err = currentErr
			result.Invalid++
		}
		result.Entries[index] = entry
	}

	trashed, err := store.scanReviewAliasTrashTargets(trashTargets, maxTrash)
	if err != nil {
		return ReviewAliasDiagnosisScan{}, err
	}
	for _, index := range missing {
		entry := &result.Entries[index]
		if _, ok := trashed[reviewAliasTrashKey(entry.Alias.PlanID, entry.Alias.AccountID)]; ok {
			entry.Status = ReviewAliasDiagnosisSoftDeleted
			result.SoftDeleted++
		} else {
			entry.Status = ReviewAliasDiagnosisOrphan
			result.Orphan++
		}
	}
	result.Issues = result.Orphan + result.SoftDeleted + result.IdentityMismatch + result.Invalid
	return result, nil
}
