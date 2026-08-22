package syncjournal

import (
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

const (
	Version            = 2
	MinReadableVersion = 1
	LayoutVersion      = "v1"
	SchemaID           = "115driver.sync-journal"
	ListEntrySchema    = "115driver.sync-journal-list-entry/v1"

	StatusActive            = "active"
	StatusFailed            = "failed"
	StatusCompleted         = "completed"
	StatusReconcileRequired = "reconcile-required"
	StatusRecoveryRequired  = "recovery-required"
	StatusUnknown           = "unknown"
)

type Postcondition struct {
	Side            string `json:"side"`
	Exists          bool   `json:"exists"`
	Kind            string `json:"kind,omitempty"`
	RemoteID        string `json:"remote_id,omitempty"`
	Size            int64  `json:"size,omitempty"`
	SHA1            string `json:"sha1,omitempty"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano,omitempty"`
}

type RunStats struct {
	Runs                int        `json:"runs"`
	ResumeRuns          int        `json:"resume_runs"`
	InterruptedRuns     int        `json:"interrupted_runs"`
	LastStartedAt       *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt      *time.Time `json:"last_finished_at,omitempty"`
	LastDurationMillis  int64      `json:"last_duration_ms"`
	TotalDurationMillis int64      `json:"total_duration_ms"`
}

type MigrationRecord struct {
	FromVersion    int       `json:"from_version"`
	ToVersion      int       `json:"to_version"`
	MigratedAt     time.Time `json:"migrated_at"`
	SourceSHA256   string    `json:"source_sha256"`
	BackupRequired bool      `json:"backup_required,omitempty"`
}

type Item struct {
	Index                 int            `json:"index"`
	RelativePath          string         `json:"relative_path"`
	Action                string         `json:"action"`
	Kind                  string         `json:"kind"`
	State                 string         `json:"state"`
	Phase                 string         `json:"phase,omitempty"`
	Attempts              int            `json:"attempts"`
	LastError             string         `json:"last_error,omitempty"`
	LocalModTimeUnixNano  int64          `json:"local_mod_time_unix_nano,omitempty"`
	RemoteModTimeUnixNano int64          `json:"remote_mod_time_unix_nano,omitempty"`
	Post                  *Postcondition `json:"postcondition,omitempty"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

type Journal struct {
	Version           int               `json:"version"`
	Schema            string            `json:"schema,omitempty"`
	MigrationRequired bool              `json:"migration_required,omitempty"`
	PlanID            string            `json:"plan_id"`
	ProfileScope      string            `json:"profile_scope"`
	AccountID         int64             `json:"account_id,omitempty"`
	State             string            `json:"state"`
	Status            string            `json:"status"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	LastError         string            `json:"last_error,omitempty"`
	RunStats          RunStats          `json:"run_stats"`
	Migrations        []MigrationRecord `json:"schema_migrations,omitempty"`
	Plan              syncplanpkg.Plan  `json:"plan"`
	Items             []Item            `json:"items"`
}

type ListEntry struct {
	Schema            string         `json:"schema"`
	PlanID            string         `json:"plan_id"`
	Version           int            `json:"version"`
	MigrationRequired bool           `json:"migration_required"`
	State             string         `json:"state"`
	Status            string         `json:"status"`
	RunStats          RunStats       `json:"run_stats"`
	Direction         string         `json:"direction"`
	ConflictPolicy    string         `json:"conflict_policy"`
	DeleteExtraneous  bool           `json:"delete"`
	LocalRoot         string         `json:"local_root"`
	RemoteRoot        string         `json:"remote_root"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	StaleForMillis    int64          `json:"stale_for_ms"`
	Total             int            `json:"total"`
	Completed         int            `json:"completed"`
	Pending           int            `json:"pending"`
	Failed            int            `json:"failed"`
	Blocked           int            `json:"blocked"`
	ActionCounts      map[string]int `json:"action_counts"`
	StateCounts       map[string]int `json:"state_counts"`
	PhaseCounts       map[string]int `json:"phase_counts"`
	RecoveryRequired  bool           `json:"recovery_required"`
	ReconcileRequired bool           `json:"reconcile_required,omitempty"`
	InUse             bool           `json:"in_use"`
}
