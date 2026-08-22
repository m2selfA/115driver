package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSyncJournalSchemaInfoExposesCompatibilityContract(t *testing.T) {
	info := currentSyncJournalSchemaInfo()
	if info.Schema != syncJournalSchemaID || info.CurrentVersion != syncJournalVersion || info.MinimumReadableVersion != syncJournalMinReadableVersion || info.LayoutVersion != syncJournalLayoutVersion {
		t.Fatalf("schema info identity/version contract: %#v", info)
	}
	if !info.LegacyReadOnly || !info.MigrationChainComplete {
		t.Fatalf("schema info safety contract: %#v", info)
	}
	if !info.MigrationSourceBackups || info.MigrationBackupDirectory != syncJournalMigrationBackupDirName || info.MigrationBatchMarkerVersion != syncJournalMigrationBatchVersion || info.MigrationBatchMinimumReadableVersion != syncJournalMigrationBatchMinReadableVersion || !info.BulkCrashRecovery {
		t.Fatalf("schema info migration recovery contract: %#v", info)
	}
	if info.ListEntrySchema != syncJournalListEntrySchema || info.VerificationResultSchema != syncJournalVerificationSchema || info.RecoveryResultSchema != syncJournalRecoveryResultSchema || info.MigrationBatchRecoveryResultSchema != syncJournalMigrationBatchRecoverySchema {
		t.Fatalf("schema info machine-result contract: %#v", info)
	}
	if len(info.ReadableVersions) != syncJournalVersion-syncJournalMinReadableVersion+1 || info.ReadableVersions[0] != syncJournalMinReadableVersion || info.ReadableVersions[len(info.ReadableVersions)-1] != syncJournalVersion {
		t.Fatalf("schema readable versions: %#v", info.ReadableVersions)
	}
	if len(info.MigrationEdges) != syncJournalVersion-syncJournalMinReadableVersion {
		t.Fatalf("schema migration edges: %#v", info.MigrationEdges)
	}
	for index, edge := range info.MigrationEdges {
		wantFrom := syncJournalMinReadableVersion + index
		if edge.From != wantFrom || edge.To != wantFrom+1 {
			t.Fatalf("schema migration edge %d: %#v", index, edge)
		}
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"schema"`, `"current_version"`, `"minimum_readable_version"`, `"layout_version"`, `"migration_edges"`, `"migration_chain_complete"`, `"migration_source_backups"`, `"migration_backup_directory"`, `"migration_batch_marker_version"`, `"migration_batch_minimum_readable_version"`, `"bulk_crash_recovery"`, `"list_entry_schema"`, `"verification_result_schema"`, `"recovery_result_schema"`, `"migration_batch_recovery_result_schema"`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("schema JSON missing %s: %s", key, encoded)
		}
	}
}

func TestSyncJournalSchemaCommandIsOfflineAndReadOnly(t *testing.T) {
	if !commandSkipsAuthentication(syncJournalSchemaCmd) {
		t.Fatal("sync journal schema unexpectedly requires authentication")
	}
	if !strings.Contains(syncJournalSchemaCmd.Long, "offline") || !strings.Contains(syncJournalSchemaCmd.Long, "does not inspect") || !strings.Contains(syncJournalSchemaCmd.Long, "rewrite") || !strings.Contains(syncJournalSchemaCmd.Long, "source-backup") || !strings.Contains(syncJournalSchemaCmd.Long, "crash-recovery") || !strings.Contains(syncJournalSchemaCmd.Long, "machine-result") {
		t.Fatalf("schema help does not document static/read-only contract: %q", syncJournalSchemaCmd.Long)
	}
}
