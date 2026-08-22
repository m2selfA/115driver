package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

var (
	ErrNotFound        = errors.New("sync execution journal not found")
	ErrExists          = errors.New("sync execution journal already exists")
	ErrBindingMismatch = errors.New("sync execution journal binding mismatch")
	ErrScanLimit       = errors.New("sync execution journal scan limit exceeded")
)

type Location struct {
	Dir         string
	JournalPath string
	LockPath    string
	LeasePath   string
}

type Store struct {
	Root           string
	ProfileScope   string
	AccountID      int64
	AutoGC         bool
	GCInterval     time.Duration
	Retention      time.Duration
	TrashRetention time.Duration
}

func normalizeProfileScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if len(scope) != 64 {
		return "", fmt.Errorf("invalid sync journal profile scope")
	}
	if _, err := NormalizePlanID(scope); err != nil {
		return "", fmt.Errorf("invalid sync journal profile scope: %w", err)
	}
	return scope, nil
}

func (store Store) RootPath() (string, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return "", errors.New("sync journal root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	scope, err := normalizeProfileScope(store.ProfileScope)
	if err != nil {
		return "", err
	}
	return filepath.Join(absolute, "sync", LayoutVersion, scope), nil
}

// MigrationBatchMarkerPresent is a conservative maintenance gate for non-CLI
// frontends. MCP cleanup refuses store-wide maintenance whenever the CLI bulk
// migration marker exists instead of attempting to interpret or modify CLI-
// owned migration evidence.
func (store Store) MigrationBatchMarkerPresent() (bool, error) {
	root, err := store.RootPath()
	if err != nil {
		return false, err
	}
	marker := filepath.Join(root, "migration", "migrate-all.json")
	if _, err := os.Lstat(marker); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, fmt.Errorf("inspect sync journal migration marker: %w", err)
	}
}

func (store Store) Location(planID string) (Location, error) {
	planID, err := NormalizePlanID(planID)
	if err != nil {
		return Location{}, err
	}
	root, err := store.RootPath()
	if err != nil {
		return Location{}, err
	}
	dir := filepath.Join(root, planID[:2], planID)
	sessionRoot := filepath.Dir(filepath.Dir(filepath.Dir(root)))
	scope, _ := normalizeProfileScope(store.ProfileScope)
	lockRoot := filepath.Join(sessionRoot, "sync-locks", scope, planID[:2])
	return Location{
		Dir: dir, JournalPath: filepath.Join(dir, "journal.json"), LeasePath: filepath.Join(dir, "lease.json"),
		LockPath: filepath.Join(lockRoot, planID+".lock"),
	}, nil
}

func (store Store) ValidateBinding(journal Journal) error {
	scope, err := normalizeProfileScope(store.ProfileScope)
	if err != nil {
		return err
	}
	if journal.ProfileScope != scope {
		return fmt.Errorf("%w: journal belongs to a different profile scope", ErrBindingMismatch)
	}
	if store.AccountID != 0 && journal.AccountID != 0 && store.AccountID != journal.AccountID {
		return fmt.Errorf("%w: journal belongs to a different account", ErrBindingMismatch)
	}
	return nil
}

func (store Store) WriteCurrent(location Location, journal Journal) (Journal, error) {
	if err := store.ValidateBinding(journal); err != nil {
		return Journal{}, err
	}
	encoded, normalized, err := EncodeCurrent(journal)
	if err != nil {
		return Journal{}, err
	}
	expectedPlanID := strings.ToLower(filepath.Base(location.Dir))
	if normalized.PlanID != expectedPlanID {
		return Journal{}, fmt.Errorf("%w: journal plan_id does not match storage path", ErrInvalidSchema)
	}
	if err := transfer.WritePrivateFileAtomic(location.JournalPath, encoded); err != nil {
		return Journal{}, fmt.Errorf("write sync execution journal: %w", err)
	}
	return normalized, nil
}

func (store Store) ReadCurrent(location Location) (Journal, error) {
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Journal{}, ErrNotFound
		}
		return Journal{}, err
	}
	journal, err := DecodeCurrent(data)
	if err != nil {
		return Journal{}, err
	}
	if err := store.ValidateBinding(journal); err != nil {
		return Journal{}, err
	}
	expectedPlanID := strings.ToLower(filepath.Base(location.Dir))
	if journal.PlanID != expectedPlanID {
		return Journal{}, fmt.Errorf("%w: journal plan_id does not match storage path", ErrInvalidSchema)
	}
	return journal, nil
}

func (store Store) InspectCurrent(planID string) (Journal, error) {
	location, err := store.Location(planID)
	if err != nil {
		return Journal{}, err
	}
	return store.ReadCurrent(location)
}

type CurrentRecord struct {
	Journal  Journal
	Location Location
	InUse    bool
}

func (store Store) InspectCurrentRecord(planID string) (CurrentRecord, error) {
	location, err := store.Location(planID)
	if err != nil {
		return CurrentRecord{}, err
	}
	journal, err := store.ReadCurrent(location)
	if err != nil {
		return CurrentRecord{}, err
	}
	inUse, err := transfer.SessionLockInUse(location.LockPath)
	if err != nil {
		return CurrentRecord{}, fmt.Errorf("inspect sync journal lock: %w", err)
	}
	return CurrentRecord{Journal: journal, Location: location, InUse: inUse}, nil
}

type CurrentScan struct {
	Records           []CurrentRecord
	MigrationRequired int
}

// ScanCurrent reads at most maxJournals journal files from this profile scope.
// Current-v2 journals are fully validated and account-bound. Readable legacy
// journals are counted but not decoded here so migration remains CLI-owned.
func (store Store) ScanCurrent(maxJournals int) (CurrentScan, error) {
	if maxJournals <= 0 {
		return CurrentScan{}, errors.New("sync journal scan limit must be > 0")
	}
	root, err := store.RootPath()
	if err != nil {
		return CurrentScan{}, err
	}
	scan := CurrentScan{Records: make([]CurrentRecord, 0)}
	seen := 0
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "journal.json" {
			return nil
		}
		seen++
		if seen > maxJournals {
			return fmt.Errorf("%w: maximum %d journal files", ErrScanLimit, maxJournals)
		}
		planID := filepath.Base(filepath.Dir(current))
		location, locErr := store.Location(planID)
		if locErr != nil {
			return fmt.Errorf("inspect sync journal candidate: %w", locErr)
		}
		journal, readErr := store.ReadCurrent(location)
		if readErr != nil {
			if errors.Is(readErr, ErrMigrationRequired) {
				scan.MigrationRequired++
				return nil
			}
			return fmt.Errorf("inspect current sync journal: %w", readErr)
		}
		inUse, lockErr := transfer.SessionLockInUse(location.LockPath)
		if lockErr != nil {
			return fmt.Errorf("inspect sync journal lock: %w", lockErr)
		}
		scan.Records = append(scan.Records, CurrentRecord{Journal: journal, Location: location, InUse: inUse})
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return CurrentScan{}, err
	}
	sort.Slice(scan.Records, func(i, j int) bool {
		if scan.Records[i].Journal.UpdatedAt.Equal(scan.Records[j].Journal.UpdatedAt) {
			return scan.Records[i].Journal.PlanID < scan.Records[j].Journal.PlanID
		}
		return scan.Records[i].Journal.UpdatedAt.After(scan.Records[j].Journal.UpdatedAt)
	})
	return scan, nil
}

type Handle struct {
	mu       sync.Mutex
	store    Store
	location Location
	lock     *transfer.SessionLock
	journal  Journal
}

func (store Store) CreateCurrent(plan syncplanpkg.Plan) (*Handle, error) {
	journal, err := New(plan, store.ProfileScope, store.AccountID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	location, err := store.Location(journal.PlanID)
	if err != nil {
		return nil, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(location.JournalPath); err == nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %s", ErrExists, journal.PlanID)
	} else if !os.IsNotExist(err) {
		_ = lock.Close()
		return nil, err
	}
	journal, err = store.WriteCurrent(location, journal)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Handle{store: store, location: location, lock: lock, journal: journal}, nil
}

func (store Store) OpenCurrent(planID string) (*Handle, error) {
	location, err := store.Location(planID)
	if err != nil {
		return nil, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return nil, err
	}
	journal, err := store.ReadCurrent(location)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Handle{store: store, location: location, lock: lock, journal: journal}, nil
}

func (handle *Handle) Close() error {
	if handle == nil || handle.lock == nil {
		return nil
	}
	err := handle.lock.Close()
	handle.lock = nil
	return err
}

func (handle *Handle) Location() Location {
	if handle == nil {
		return Location{}
	}
	return handle.location
}

func (handle *Handle) Snapshot() Journal {
	if handle == nil {
		return Journal{}
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	return Clone(handle.journal)
}

func (handle *Handle) Mutate(fn func(*Journal) error) error {
	if handle == nil {
		return errors.New("sync journal handle is nil")
	}
	if fn == nil {
		return errors.New("sync journal mutation is nil")
	}
	handle.mu.Lock()
	defer handle.mu.Unlock()
	next := Clone(handle.journal)
	if err := fn(&next); err != nil {
		return err
	}
	next.UpdatedAt = time.Now().UTC()
	next.Status = EffectiveStatus(next)
	written, err := handle.store.WriteCurrent(handle.location, next)
	if err != nil {
		return err
	}
	handle.journal = written
	return nil
}
