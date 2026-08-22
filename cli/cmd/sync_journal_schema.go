package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

type syncJournalSchemaMigrationEdge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type syncJournalSchemaInfo struct {
	Schema                               string                           `json:"schema"`
	CurrentVersion                       int                              `json:"current_version"`
	MinimumReadableVersion               int                              `json:"minimum_readable_version"`
	LayoutVersion                        string                           `json:"layout_version"`
	ReadableVersions                     []int                            `json:"readable_versions"`
	MigrationEdges                       []syncJournalSchemaMigrationEdge `json:"migration_edges"`
	MigrationChainComplete               bool                             `json:"migration_chain_complete"`
	LegacyReadOnly                       bool                             `json:"legacy_read_only"`
	MigrationSourceBackups               bool                             `json:"migration_source_backups"`
	MigrationBackupDirectory             string                           `json:"migration_backup_directory"`
	MigrationBatchMarkerVersion          int                              `json:"migration_batch_marker_version"`
	MigrationBatchMinimumReadableVersion int                              `json:"migration_batch_minimum_readable_version"`
	ListEntrySchema                      string                           `json:"list_entry_schema"`
	VerificationResultSchema             string                           `json:"verification_result_schema"`
	RecoveryResultSchema                 string                           `json:"recovery_result_schema"`
	MigrationBatchRecoveryResultSchema   string                           `json:"migration_batch_recovery_result_schema"`
	BulkCrashRecovery                    bool                             `json:"bulk_crash_recovery"`
}

func currentSyncJournalSchemaInfo() syncJournalSchemaInfo {
	info := syncJournalSchemaInfo{
		Schema: syncJournalSchemaID, CurrentVersion: syncJournalVersion, MinimumReadableVersion: syncJournalMinReadableVersion,
		LayoutVersion: syncJournalLayoutVersion, LegacyReadOnly: syncJournalMinReadableVersion < syncJournalVersion,
		MigrationSourceBackups: true, MigrationBackupDirectory: syncJournalMigrationBackupDirName,
		MigrationBatchMarkerVersion: syncJournalMigrationBatchVersion, MigrationBatchMinimumReadableVersion: syncJournalMigrationBatchMinReadableVersion, BulkCrashRecovery: true,
		ListEntrySchema: syncJournalListEntrySchema, VerificationResultSchema: syncJournalVerificationSchema,
		RecoveryResultSchema: syncJournalRecoveryResultSchema, MigrationBatchRecoveryResultSchema: syncJournalMigrationBatchRecoverySchema,
		ReadableVersions:       make([]int, 0, syncJournalVersion-syncJournalMinReadableVersion+1),
		MigrationEdges:         make([]syncJournalSchemaMigrationEdge, 0, len(syncJournalMigrationSteps)),
		MigrationChainComplete: true,
	}
	for version := syncJournalMinReadableVersion; version <= syncJournalVersion; version++ {
		info.ReadableVersions = append(info.ReadableVersions, version)
	}
	for from := range syncJournalMigrationSteps {
		if from >= syncJournalMinReadableVersion && from < syncJournalVersion {
			info.MigrationEdges = append(info.MigrationEdges, syncJournalSchemaMigrationEdge{From: from, To: from + 1})
		}
	}
	sort.Slice(info.MigrationEdges, func(i, j int) bool { return info.MigrationEdges[i].From < info.MigrationEdges[j].From })
	for version := syncJournalMinReadableVersion; version < syncJournalVersion; version++ {
		if syncJournalMigrationSteps[version] == nil {
			info.MigrationChainComplete = false
			break
		}
	}
	return info
}

func printSyncJournalSchemaInfo(info syncJournalSchemaInfo) {
	if jsonOutput {
		return
	}
	fmt.Printf("Sync journal schema: %s v%d (readable v%d..v%d; layout=%s; migration-chain-complete=%t; source-backups=%t; bulk-crash-recovery=%t; batch-marker-readable=v%d..v%d)\n",
		info.Schema, info.CurrentVersion, info.MinimumReadableVersion, info.CurrentVersion, info.LayoutVersion, info.MigrationChainComplete, info.MigrationSourceBackups, info.BulkCrashRecovery, info.MigrationBatchMinimumReadableVersion, info.MigrationBatchMarkerVersion)
	fmt.Printf("machine-results: list=%s verify=%s recover=%s batch-recover=%s\n", info.ListEntrySchema, info.VerificationResultSchema, info.RecoveryResultSchema, info.MigrationBatchRecoveryResultSchema)
	for _, edge := range info.MigrationEdges {
		fmt.Printf("migration: v%d -> v%d\n", edge.From, edge.To)
	}
}

var syncJournalSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Show sync journal schema and migration compatibility",
	Long:  "Show the static sync journal schema identity, migration compatibility, current and minimum-readable versions, on-disk layout version, registered migration edges, migration source-backup capability, and bulk crash-recovery marker readable/current versions, and stable machine-result schema identifiers. This command is offline and does not inspect, rewrite, migrate, or lock any journal.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		info := currentSyncJournalSchemaInfo()
		printer.PrintSuccess(info)
		printSyncJournalSchemaInfo(info)
		return nil
	},
}
