package syncjournal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

var (
	ErrMigrationRequired  = errors.New("sync execution journal requires schema migration")
	ErrNewerVersion       = errors.New("sync execution journal was created by a newer 115driver")
	ErrUnsupportedVersion = errors.New("sync execution journal schema version is unsupported")
)

func validateCurrentEnvelope(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: decode schema envelope: %v", ErrInvalidSchema, err)
	}
	schemaRaw, ok := raw["schema"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing schema identity", ErrInvalidSchema, Version)
	}
	var schema string
	if err := json.Unmarshal(schemaRaw, &schema); err != nil || schema != SchemaID {
		return fmt.Errorf("%w: schema v%d identity must be %q", ErrInvalidSchema, Version, SchemaID)
	}
	statusRaw, ok := raw["status"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing required field %q", ErrInvalidSchema, Version, "status")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil || status == "" {
		return fmt.Errorf("%w: schema v%d field %q must be a non-empty string", ErrInvalidSchema, Version, "status")
	}
	runStatsRaw, ok := raw["run_stats"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing required field %q", ErrInvalidSchema, Version, "run_stats")
	}
	var runStats map[string]json.RawMessage
	if err := json.Unmarshal(runStatsRaw, &runStats); err != nil || runStats == nil {
		return fmt.Errorf("%w: schema v%d field %q must be an object", ErrInvalidSchema, Version, "run_stats")
	}
	for _, key := range []string{"runs", "resume_runs", "interrupted_runs", "last_duration_ms", "total_duration_ms"} {
		if _, ok := runStats[key]; !ok {
			return fmt.Errorf("%w: schema v%d field %q is missing %q", ErrInvalidSchema, Version, "run_stats", key)
		}
	}
	return nil
}

func ValidateMigrationHistory(journal Journal) error {
	previousTo := 0
	for index, record := range journal.Migrations {
		if record.FromVersion < MinReadableVersion || record.ToVersion != record.FromVersion+1 || record.ToVersion > journal.Version {
			return fmt.Errorf("%w: sync journal schema migration %d has invalid version edge %d -> %d", ErrInvalidSchema, index, record.FromVersion, record.ToVersion)
		}
		if previousTo != 0 && record.FromVersion != previousTo {
			return fmt.Errorf("%w: sync journal schema migration %d is not contiguous", ErrInvalidSchema, index)
		}
		if record.MigratedAt.IsZero() {
			return fmt.Errorf("%w: sync journal schema migration %d is missing migrated_at", ErrInvalidSchema, index)
		}
		digest, err := hex.DecodeString(record.SourceSHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("%w: sync journal schema migration %d has invalid source_sha256", ErrInvalidSchema, index)
		}
		previousTo = record.ToVersion
	}
	if len(journal.Migrations) > 0 && previousTo != journal.Version {
		return fmt.Errorf("%w: sync journal schema migration history ends at version %d but journal is version %d", ErrInvalidSchema, previousTo, journal.Version)
	}
	return nil
}

// RestoreCurrent validates one current-schema journal and restores the hidden
// plan snapshot fields persisted on journal items. It performs no I/O.
func RestoreCurrent(journal *Journal) error {
	if journal == nil {
		return errors.New("sync journal is nil")
	}
	if journal.Version > Version {
		return fmt.Errorf("%w: version %d is newer than supported version %d", ErrNewerVersion, journal.Version, Version)
	}
	if journal.Version < Version {
		if journal.Version >= MinReadableVersion {
			return fmt.Errorf("%w: have version %d, need version %d", ErrMigrationRequired, journal.Version, Version)
		}
		return fmt.Errorf("%w: version %d", ErrUnsupportedVersion, journal.Version)
	}
	if journal.Schema != SchemaID {
		return fmt.Errorf("%w: schema v%d requires identity %q", ErrInvalidSchema, journal.Version, SchemaID)
	}
	if err := ValidateJournalState(journal.State); err != nil {
		return err
	}
	planID, err := NormalizePlanID(journal.PlanID)
	if err != nil {
		return fmt.Errorf("invalid sync journal plan ID: %w", err)
	}
	if len(journal.Items) != len(journal.Plan.Items) {
		return fmt.Errorf("%w: sync journal item count does not match stored plan", ErrInvalidSchema)
	}
	for index := range journal.Plan.Items {
		if err := RestoreStoredItem(index, journal.Items[index], &journal.Plan.Items[index]); err != nil {
			return err
		}
	}
	if fingerprint := syncplanpkg.Fingerprint(journal.Plan); fingerprint != planID {
		return fmt.Errorf("%w: sync journal stored plan fingerprint changed", ErrInvalidSchema)
	}
	journal.PlanID = planID
	journal.Plan.PlanID = planID
	journal.MigrationRequired = false
	journal.Status = EffectiveStatus(*journal)
	return nil
}

// DecodeCurrent accepts only the current schema. Legacy readable journals are
// deliberately surfaced as ErrMigrationRequired so only the CLI migration path
// can rewrite historical on-disk state.
func DecodeCurrent(data []byte) (Journal, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Journal{}, fmt.Errorf("decode sync execution journal header: %w", err)
	}
	if header.Version > Version {
		return Journal{}, fmt.Errorf("%w: version %d is newer than supported version %d", ErrNewerVersion, header.Version, Version)
	}
	if header.Version < Version {
		if header.Version >= MinReadableVersion {
			return Journal{}, fmt.Errorf("%w: have version %d, need version %d", ErrMigrationRequired, header.Version, Version)
		}
		return Journal{}, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, header.Version)
	}
	if err := validateCurrentEnvelope(data); err != nil {
		return Journal{}, err
	}
	var journal Journal
	if err := json.Unmarshal(data, &journal); err != nil {
		return Journal{}, fmt.Errorf("decode sync execution journal: %w", err)
	}
	storedStatus := journal.Status
	storedMigrationRequired := journal.MigrationRequired
	if err := RestoreCurrent(&journal); err != nil {
		return Journal{}, err
	}
	if storedStatus != journal.Status {
		return Journal{}, fmt.Errorf("%w: persisted status %q does not match derived status %q", ErrInvalidSchema, storedStatus, journal.Status)
	}
	if storedMigrationRequired {
		return Journal{}, fmt.Errorf("%w: current schema v%d cannot be marked migration_required", ErrInvalidSchema, header.Version)
	}
	if err := ValidateRunStats(journal.RunStats); err != nil {
		return Journal{}, err
	}
	if err := ValidateMigrationHistory(journal); err != nil {
		return Journal{}, err
	}
	return journal, nil
}

func EncodeCurrent(journal Journal) ([]byte, Journal, error) {
	journal.Version = Version
	journal.Schema = SchemaID
	journal.MigrationRequired = false
	journal.Status = EffectiveStatus(journal)
	checked := Clone(journal)
	if err := RestoreCurrent(&checked); err != nil {
		return nil, Journal{}, err
	}
	if err := ValidateRunStats(checked.RunStats); err != nil {
		return nil, Journal{}, err
	}
	if err := ValidateMigrationHistory(checked); err != nil {
		return nil, Journal{}, err
	}
	encoded, err := json.MarshalIndent(checked, "", "  ")
	if err != nil {
		return nil, Journal{}, fmt.Errorf("encode sync execution journal: %w", err)
	}
	return encoded, checked, nil
}
