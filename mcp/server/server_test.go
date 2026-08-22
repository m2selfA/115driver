package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func assertSchemaOmitsKeys(t *testing.T, schema any, forbidden ...string) {
	t.Helper()
	if schema == nil {
		t.Fatal("expected a non-nil output schema")
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal output schema: %v", err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatalf("decode output schema: %v", err)
	}
	forbiddenSet := make(map[string]struct{}, len(forbidden))
	for _, key := range forbidden {
		forbiddenSet[strings.ToLower(key)] = struct{}{}
	}
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, blocked := forbiddenSet[strings.ToLower(key)]; blocked {
					t.Fatalf("output schema exposes forbidden key %q: %s", key, encoded)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
}

func mustMarshalMCPToolInputSchema(t *testing.T, tool *mcp.Tool) []byte {
	t.Helper()
	if tool == nil {
		t.Fatal("expected MCP tool for input schema marshal")
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal %s input schema: %v", tool.Name, err)
	}
	return encoded
}

func TestDefaultReadOnlyOutputSchemasAvoidCredentialBearingFields(t *testing.T) {
	forbidden := []string{"url", "thumb_url", "face", "receive_code", "share_code", "imei_info", "cookie", "header", "signed_url"}
	for _, tool := range registeredTools(t, false, false, false) {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("default tool %s is not annotated read-only: %#v", tool.Name, tool.Annotations)
		}
		t.Run(tool.Name, func(t *testing.T) {
			assertSchemaOmitsKeys(t, tool.OutputSchema, forbidden...)
		})
	}
}

func TestServerVersionUsesLinkedRelease(t *testing.T) {
	previous := version
	version = "1.4.0"
	defer func() { version = previous }()

	if got := serverVersion(); got != "1.4.0" {
		t.Fatalf("serverVersion = %q, want 1.4.0", got)
	}
}

func TestNewServerPreservesTransferDefaults(t *testing.T) {
	s := NewServer()
	if s.downloadTimeout != 2*time.Hour {
		t.Fatalf("unexpected default download timeout: %s", s.downloadTimeout)
	}
	if s.urlUploadMaxBytes != 2<<30 {
		t.Fatalf("unexpected default URL upload limit: %d", s.urlUploadMaxBytes)
	}
	if s.downloadMaxBytes != 0 {
		t.Fatalf("expected default download limit to be disabled, got %d", s.downloadMaxBytes)
	}
}

func TestStartRejectsInvalidLocalRootBeforeStartingTransport(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	err := NewServer().WithLocalRoot(missing).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid MCP local root") {
		t.Fatalf("Start(invalid local root) error = %v", err)
	}
}

func TestStartRejectsMissingDriverClientBeforeStartingTransport(t *testing.T) {
	err := NewServer().Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "driver client is required") {
		t.Fatalf("Start(missing client) error = %v", err)
	}
}

func TestRegisteredToolSurfaceMatchesSafetyMode(t *testing.T) {
	wantRemoteOnly := []string{
		"getAccountInfo",
		"get_app_versions",
		"getShareSnap",
		"get_share_snaps",
		"compare_directories",
		"inspect_paths",
		"listDirectory",
		"list_directories",
		"list_tree",
		"resolve_paths",
		"summarize_usage",
		"listOfflineTasks",
		"list_offline_pages",
		"listRecycleBin",
		"list_recycle_pages",
		"search",
		"search_many",
		"stat",
		"stat_many",
		"validate_plan",
	}
	assertToolNamesEqual(t, registeredToolNames(t, false, false, false), wantRemoteOnly)

	wantWithLocal := append(append([]string{}, wantRemoteOnly...), "diagnose_sync_journal_aliases", "diagnose_sync_recovery", "download_file", "download_files", "download_share_file", "download_share_files", "inspect_sync_journal", "list_sync_executions", "list_sync_journal_trash", "plan_sync", "plan_sync_journal_alias_repair", "plan_sync_journal_cleanup", "plan_transfer", "revalidate_sync_plan", "revalidate_transfer_plan")
	assertToolNamesEqual(t, registeredToolNames(t, false, false, true), wantWithLocal)

	wantWithSensitive := append(append([]string{}, wantRemoteOnly...), "get_download_info")
	assertToolNamesEqual(t, registeredToolNames(t, false, true, false), wantWithSensitive)

	wantDestructiveRemoteOnly := append(append([]string{}, wantRemoteOnly...),
		"addOfflineTaskURIs",
		"cleanRecycleBin",
		"clearOfflineTasks",
		"copy",
		"delete",
		"deleteOfflineTasks",
		"mkdir",
		"mkdir_many",
		"move",
		"rename",
		"rename_many",
		"revertRecycleBin",
		"upload_from_url",
		"upload_from_urls",
	)
	assertToolNamesEqual(t, registeredToolNames(t, true, false, false), wantDestructiveRemoteOnly)

	wantDestructiveWithLocal := append(append([]string{}, wantDestructiveRemoteOnly...), "diagnose_sync_journal_aliases", "diagnose_sync_recovery", "download_file", "download_files", "download_share_file", "download_share_files", "execute_sync_journal_alias_repair", "execute_sync_journal_cleanup", "execute_sync_plan", "execute_transfer_plan", "inspect_sync_journal", "list_sync_executions", "list_sync_journal_trash", "plan_sync", "plan_sync_journal_alias_repair", "plan_sync_journal_cleanup", "plan_transfer", "reconcile_sync_journal_alias", "reconcile_sync_recovery", "restore_sync_journal", "revalidate_sync_plan", "revalidate_transfer_plan", "upload_from_local", "upload_from_local_files")
	assertToolNamesEqual(t, registeredToolNames(t, true, false, true), wantDestructiveWithLocal)

	wantAllCapabilities := append(append([]string{}, wantDestructiveWithLocal...), "get_download_info")
	assertToolNamesEqual(t, registeredToolNames(t, true, true, true), wantAllCapabilities)
}

func TestRegisteredToolSchemasExposeSafeParameters(t *testing.T) {
	tools := registeredTools(t, true, false, true)
	byName := make(map[string]*mcp.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	clearTool := byName["clearOfflineTasks"]
	if clearTool == nil {
		t.Fatal("clearOfflineTasks is missing from destructive MCP surface")
	}
	clearSchema, err := json.Marshal(clearTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal clearOfflineTasks input schema: %v", err)
	}
	clearText := string(clearSchema)
	for _, expected := range []string{"scope", "clear_flag", "dry_run", "completed", "failed", "active", "all"} {
		if !strings.Contains(clearText, expected) {
			t.Fatalf("clearOfflineTasks input schema lost %q: %s", expected, clearText)
		}
	}

	urlUploadBatchTool := byName["upload_from_urls"]
	if urlUploadBatchTool == nil {
		t.Fatal("upload_from_urls is missing from destructive remote MCP surface")
	}
	urlUploadBatchInput, err := json.Marshal(urlUploadBatchTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal upload_from_urls input schema: %v", err)
	}
	for _, expected := range []string{"files", "url", "dir_id", "file_name"} {
		if !strings.Contains(string(urlUploadBatchInput), expected) {
			t.Fatalf("upload_from_urls input schema lost %q: %s", expected, urlUploadBatchInput)
		}
	}
	if urlUploadBatchTool.OutputSchema == nil {
		t.Fatal("upload_from_urls is missing typed output schema")
	}
	assertSchemaOmitsKeys(t, urlUploadBatchTool.OutputSchema, "source_url", "url", "local_path", "sha1", "network", "endpoint", "cookie", "header")

	uploadBatchTool := byName["upload_from_local_files"]
	if uploadBatchTool == nil {
		t.Fatal("upload_from_local_files is missing when destructive and local file capabilities are enabled")
	}
	uploadBatchInput, err := json.Marshal(uploadBatchTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal upload_from_local_files input schema: %v", err)
	}
	for _, expected := range []string{"files", "local_path", "dir_id", "file_name", "expect_plan_id", "max_checksum_bytes"} {
		if !strings.Contains(string(uploadBatchInput), expected) {
			t.Fatalf("upload_from_local_files input schema lost %q: %s", expected, uploadBatchInput)
		}
	}
	if uploadBatchTool.OutputSchema == nil {
		t.Fatal("upload_from_local_files is missing typed output schema")
	}
	assertSchemaOmitsKeys(t, uploadBatchTool.OutputSchema, "local_path", "sha1", "network", "endpoint", "cookie", "header")

	for toolName, expectedFields := range map[string][]string{
		"list_directories":                  {"directories", "dir_id", "offset", "limit"},
		"compare_directories":               {"left_path", "right_path", "max_depth", "max_nodes", "include_unchanged"},
		"list_tree":                         {"paths", "max_depth", "max_nodes"},
		"resolve_paths":                     {"paths"},
		"inspect_paths":                     {"paths"},
		"inspect_sync_journal":              {"plan_id"},
		"diagnose_sync_recovery":            {"plan_id"},
		"diagnose_sync_journal_aliases":     {"limit"},
		"plan_sync_journal_alias_repair":    {"limit"},
		"list_sync_executions":              {"limit", "status"},
		"list_sync_journal_trash":           {"limit"},
		"plan_sync_journal_cleanup":         {"older_than_hours", "limit"},
		"list_offline_pages":                {"pages"},
		"list_recycle_pages":                {"pages", "offset", "limit"},
		"summarize_usage":                   {"paths", "max_depth", "max_nodes"},
		"search_many":                       {"queries", "search_value", "offset", "limit", "type", "order", "asc"},
		"get_share_snaps":                   {"requests", "share_code", "receive_code", "dir_id", "offset", "limit"},
		"stat_many":                         {"file_ids"},
		"plan_sync":                         {"local_path", "remote_path", "direction", "conflict_policy", "delete", "max_nodes", "max_checksum_bytes"},
		"revalidate_sync_plan":              {"local_path", "remote_path", "direction", "conflict_policy", "delete", "max_nodes", "max_checksum_bytes", "expect_plan_id"},
		"plan_transfer":                     {"uploads", "downloads", "local_path", "dir_id", "file_name", "pick_code", "user_agent", "max_checksum_bytes"},
		"revalidate_transfer_plan":          {"uploads", "downloads", "local_path", "dir_id", "file_name", "pick_code", "user_agent", "max_checksum_bytes", "expect_plan_id"},
		"execute_transfer_plan":             {"uploads", "downloads", "local_path", "dir_id", "file_name", "pick_code", "user_agent", "expect_plan_id", "max_checksum_bytes"},
		"execute_sync_plan":                 {"local_path", "remote_path", "direction", "conflict_policy", "delete", "max_nodes", "max_checksum_bytes", "max_delete_roots", "max_delete_items", "max_delete_bytes", "expect_plan_id", "jobs", "continue_on_error", "max_errors"},
		"execute_sync_journal_cleanup":      {"older_than_hours", "limit", "expect_cleanup_id"},
		"execute_sync_journal_alias_repair": {"limit", "expect_repair_set_id"},
		"reconcile_sync_recovery":           {"plan_id", "expect_diagnosis_id"},
		"reconcile_sync_journal_alias":      {"plan_id", "expect_repair_id"},
		"restore_sync_journal":              {"plan_id", "expect_restore_id"},
		"validate_plan":                     {"plan", "plan_id", "operations", "dependencies", "preconditions", "safety_class"},
		"delete":                            {"file_ids", "dry_run"},
		"move":                              {"dir_id", "file_ids", "dry_run"},
		"copy":                              {"dir_id", "file_ids", "dry_run"},
		"mkdir_many":                        {"directories", "parent_id", "name", "dry_run", "continue_on_error"},
		"rename_many":                       {"files", "file_id", "new_name", "dry_run", "continue_on_error"},
		"revertRecycleBin":                  {"item_ids", "dry_run"},
		"cleanRecycleBin":                   {"password", "item_ids", "dry_run"},
		"addOfflineTaskURIs":                {"uris", "save_dir_id", "dry_run"},
		"deleteOfflineTasks":                {"hashes", "delete_files", "dry_run"},
		"clearOfflineTasks":                 {"scope", "clear_flag", "dry_run"},
		"upload_from_url":                   {"url", "dir_id", "file_name", "dry_run"},
		"upload_from_urls":                  {"files", "url", "dir_id", "file_name", "dry_run"},
		"upload_from_local":                 {"local_path", "dir_id", "file_name", "dry_run", "expect_plan_id", "max_checksum_bytes"},
		"upload_from_local_files":           {"files", "local_path", "dir_id", "file_name", "dry_run", "expect_plan_id", "max_checksum_bytes"},
	} {
		tool := byName[toolName]
		if tool == nil {
			t.Fatalf("%s is missing from destructive MCP surface", toolName)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", toolName, err)
		}
		for _, field := range expectedFields {
			if !strings.Contains(string(schema), field) {
				t.Fatalf("%s input schema lost %q: %s", toolName, field, schema)
			}
		}
	}

	for _, toolName := range []string{"stat_many", "list_directories", "list_tree", "resolve_paths", "inspect_paths", "summarize_usage", "search_many", "get_share_snaps", "list_offline_pages", "list_recycle_pages"} {
		tool := byName[toolName]
		if tool == nil || tool.OutputSchema == nil {
			t.Fatalf("%s is missing typed output schema", toolName)
		}
		encoded, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatalf("marshal %s output schema: %v", toolName, err)
		}
		for _, expected := range []string{"requested", "succeeded", "failed", "items"} {
			if !strings.Contains(string(encoded), expected) {
				t.Fatalf("%s output schema lost %q: %s", toolName, expected, encoded)
			}
		}
	}
	offlinePagesOutput, err := json.Marshal(byName["list_offline_pages"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal list_offline_pages output schema: %v", err)
	}
	if !strings.Contains(string(offlinePagesOutput), "next_page") {
		t.Fatalf("list_offline_pages output schema lost next_page: %s", offlinePagesOutput)
	}

	for _, toolName := range []string{"getAccountInfo", "get_app_versions", "getShareSnap", "get_share_snaps", "compare_directories", "diagnose_sync_journal_aliases", "diagnose_sync_recovery", "inspect_paths", "inspect_sync_journal", "list_sync_executions", "list_sync_journal_trash", "listDirectory", "list_directories", "list_tree", "resolve_paths", "summarize_usage", "listOfflineTasks", "list_offline_pages", "listRecycleBin", "list_recycle_pages", "plan_sync", "plan_sync_journal_alias_repair", "plan_sync_journal_cleanup", "plan_transfer", "revalidate_sync_plan", "revalidate_transfer_plan", "search", "search_many", "stat", "stat_many", "validate_plan"} {
		tool := byName[toolName]
		if tool == nil || tool.OutputSchema == nil {
			t.Fatalf("default read-only tool %s is missing typed output schema", toolName)
		}
	}
	appVersionsSchema, err := json.Marshal(byName["get_app_versions"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal get_app_versions output schema: %v", err)
	}
	for _, expected := range []string{"count", "versions", "app", "version"} {
		if !strings.Contains(string(appVersionsSchema), expected) {
			t.Fatalf("get_app_versions output schema lost %q: %s", expected, appVersionsSchema)
		}
	}

	assertSchemaOmitsKeys(t, byName["getAccountInfo"].OutputSchema, "imei_info")
	assertSchemaOmitsKeys(t, byName["getShareSnap"].OutputSchema, "receive_code", "share_code", "face", "u")
	assertSchemaOmitsKeys(t, byName["get_share_snaps"].OutputSchema, "receive_code", "share_code", "face", "u")
	assertSchemaOmitsKeys(t, byName["inspect_paths"].OutputSchema, "url", "thumb_url", "u")
	assertSchemaOmitsKeys(t, byName["inspect_sync_journal"].OutputSchema, "local_path", "remote_path", "remote_id", "sha1", "postcondition", "last_error", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["diagnose_sync_recovery"].OutputSchema, "local_path", "remote_path", "remote_id", "sha1", "postcondition", "last_error", "journal_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["diagnose_sync_journal_aliases"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["plan_sync_journal_alias_repair"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["execute_sync_journal_alias_repair"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["reconcile_sync_journal_alias"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["reconcile_sync_recovery"].OutputSchema, "local_path", "remote_path", "remote_id", "sha1", "postcondition", "last_error", "journal_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["list_sync_executions"].OutputSchema, "local_path", "remote_path", "remote_id", "sha1", "postcondition", "last_error", "journal_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["list_sync_journal_trash"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["restore_sync_journal"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "raw_plan_id", "trash_name", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["plan_sync_journal_cleanup"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "sha1", "postcondition", "last_error", "journal_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["execute_sync_journal_cleanup"].OutputSchema, "local_path", "remote_path", "remote_id", "internal_plan_id", "sha1", "postcondition", "last_error", "journal_path", "trash_path", "profile_scope", "account_id", "pick_code", "url", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["compare_directories"].OutputSchema, "url", "thumb_url", "u")
	assertSchemaOmitsKeys(t, byName["plan_sync"].OutputSchema, "local_path", "sha1", "pick_code", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["revalidate_sync_plan"].OutputSchema, "local_path", "remote_path", "snapshot_id", "sha1", "pick_code", "source_ref", "target_ref", "ref", "expected", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["plan_transfer"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["revalidate_transfer_plan"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header", "source_ref", "target_ref", "ref", "expected")
	assertSchemaOmitsKeys(t, byName["execute_transfer_plan"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["execute_sync_plan"].OutputSchema, "local_path", "remote_path", "remote_id", "snapshot_id", "sha1", "local_sha1", "remote_sha1", "pick_code", "source_ref", "target_ref", "ref", "expected", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["validate_plan"].OutputSchema, "source_ref", "target_ref", "ref", "expected")
	diagnoseRecoverySchema, err := json.Marshal(byName["diagnose_sync_recovery"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal diagnose_sync_recovery output schema: %v", err)
	}
	for _, expected := range []string{"diagnosis_id", "resolvable", "checked", "completed", "retry_full", "winner_only", "pending_observation", "ambiguous", "checksum_budget_bytes", "checksummed_bytes"} {
		if !strings.Contains(string(diagnoseRecoverySchema), expected) {
			t.Fatalf("diagnose_sync_recovery output schema lost %q: %s", expected, diagnoseRecoverySchema)
		}
	}
	reconcileRecoverySchema, err := json.Marshal(byName["reconcile_sync_recovery"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal reconcile_sync_recovery output schema: %v", err)
	}
	for _, expected := range []string{"applied", "diagnosis_id", "completed", "retry_full", "winner_only", "pending_observation", "resume_candidate", "error_code"} {
		if !strings.Contains(string(reconcileRecoverySchema), expected) {
			t.Fatalf("reconcile_sync_recovery output schema lost %q: %s", expected, reconcileRecoverySchema)
		}
	}
	cleanupPlanSchema, err := json.Marshal(byName["plan_sync_journal_cleanup"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal plan_sync_journal_cleanup output schema: %v", err)
	}
	for _, expected := range []string{"cleanup_id", "retention_ms", "eligible", "selected", "plan_id", "updated_at", "stale_for_ms"} {
		if !strings.Contains(string(cleanupPlanSchema), expected) {
			t.Fatalf("plan_sync_journal_cleanup output schema lost %q: %s", expected, cleanupPlanSchema)
		}
	}
	executeCleanupSchema, err := json.Marshal(byName["execute_sync_journal_cleanup"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal execute_sync_journal_cleanup output schema: %v", err)
	}
	for _, expected := range []string{"cleanup_id", "requested", "trashed", "failed", "skipped", "partial", "plan_id", "status", "error_code"} {
		if !strings.Contains(string(executeCleanupSchema), expected) {
			t.Fatalf("execute_sync_journal_cleanup output schema lost %q: %s", expected, executeCleanupSchema)
		}
	}
	aliasDiagnosisSchema, err := json.Marshal(byName["diagnose_sync_journal_aliases"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal diagnose_sync_journal_aliases output schema: %v", err)
	}
	for _, expected := range []string{"scanned", "returned", "live", "orphan", "soft_deleted", "identity_mismatch", "invalid", "issues", "plan_id", "status", "repair_id", "in_use", "error_code"} {
		if !strings.Contains(string(aliasDiagnosisSchema), expected) {
			t.Fatalf("diagnose_sync_journal_aliases output schema lost %q: %s", expected, aliasDiagnosisSchema)
		}
	}
	reconcileAliasSchema, err := json.Marshal(byName["reconcile_sync_journal_alias"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal reconcile_sync_journal_alias output schema: %v", err)
	}
	for _, expected := range []string{"plan_id", "repair_id", "repaired", "status", "error_code", "error"} {
		if !strings.Contains(string(reconcileAliasSchema), expected) {
			t.Fatalf("reconcile_sync_journal_alias output schema lost %q: %s", expected, reconcileAliasSchema)
		}
	}
	aliasRepairPlanSchema, err := json.Marshal(byName["plan_sync_journal_alias_repair"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal plan_sync_journal_alias_repair output schema: %v", err)
	}
	for _, expected := range []string{"repair_set_id", "scanned", "eligible", "selected", "items", "plan_id", "repair_id", "error_code"} {
		if !strings.Contains(string(aliasRepairPlanSchema), expected) {
			t.Fatalf("plan_sync_journal_alias_repair output schema lost %q: %s", expected, aliasRepairPlanSchema)
		}
	}
	aliasRepairExecuteSchema, err := json.Marshal(byName["execute_sync_journal_alias_repair"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal execute_sync_journal_alias_repair output schema: %v", err)
	}
	for _, expected := range []string{"repair_set_id", "requested", "removed", "unchanged", "unknown", "partial", "recovery_required", "items", "plan_id", "status", "error_code"} {
		if !strings.Contains(string(aliasRepairExecuteSchema), expected) {
			t.Fatalf("execute_sync_journal_alias_repair output schema lost %q: %s", expected, aliasRepairExecuteSchema)
		}
	}
	trashListSchema, err := json.Marshal(byName["list_sync_journal_trash"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal list_sync_journal_trash output schema: %v", err)
	}
	for _, expected := range []string{"restore_id", "plan_id", "trashed_at", "updated_at", "trash_age_ms", "trash_retention_ms", "purge_eligible_at", "purge_eligible", "state", "status", "recovery_required", "reconcile_required"} {
		if !strings.Contains(string(trashListSchema), expected) {
			t.Fatalf("list_sync_journal_trash output schema lost %q: %s", expected, trashListSchema)
		}
	}
	restoreSchema, err := json.Marshal(byName["restore_sync_journal"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal restore_sync_journal output schema: %v", err)
	}
	for _, expected := range []string{"restored", "restore_id", "plan_id", "state", "status", "recovery_required", "reconcile_required", "error_code"} {
		if !strings.Contains(string(restoreSchema), expected) {
			t.Fatalf("restore_sync_journal output schema lost %q: %s", expected, restoreSchema)
		}
	}
	planSyncSchema, err := json.Marshal(byName["plan_sync"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal plan_sync output schema: %v", err)
	}
	for _, expected := range []string{"summary", "snapshot_id", "plan_version", "plan_id", "operations", "dependencies", "preconditions", "safety_class", "items"} {
		if !strings.Contains(string(planSyncSchema), expected) {
			t.Fatalf("plan_sync output schema lost %q: %s", expected, planSyncSchema)
		}
	}
	revalidateSyncSchema, err := json.Marshal(byName["revalidate_sync_plan"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal revalidate_sync_plan output schema: %v", err)
	}
	for _, expected := range []string{"matches", "ready", "gate_satisfied", "plan_id", "safety_class", "operation_count", "destructive_actions", "requires_allow_destructive", "checksummed_files", "checksummed_bytes", "error_code", "error"} {
		if !strings.Contains(string(revalidateSyncSchema), expected) {
			t.Fatalf("revalidate_sync_plan output schema lost %q: %s", expected, revalidateSyncSchema)
		}
	}
	planTransferSchema, err := json.Marshal(byName["plan_transfer"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal plan_transfer output schema: %v", err)
	}
	for _, expected := range []string{"summary", "requested", "uploads", "downloads", "known_transfer_bytes", "checksummed_files", "checksummed_bytes", "plan_version", "plan_id", "operations", "preconditions", "safety_class", "items", "target_exists"} {
		if !strings.Contains(string(planTransferSchema), expected) {
			t.Fatalf("plan_transfer output schema lost %q: %s", expected, planTransferSchema)
		}
	}
	revalidateTransferSchema, err := json.Marshal(byName["revalidate_transfer_plan"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal revalidate_transfer_plan output schema: %v", err)
	}
	for _, expected := range []string{"matches", "gate_satisfied", "plan_id", "safety_class", "operation_count", "existing_local_targets", "known_transfer_bytes", "unknown_size_transfers", "checksummed_files", "checksummed_bytes", "error_code", "error"} {
		if !strings.Contains(string(revalidateTransferSchema), expected) {
			t.Fatalf("revalidate_transfer_plan output schema lost %q: %s", expected, revalidateTransferSchema)
		}
	}
	executeTransferSchema, err := json.Marshal(byName["execute_transfer_plan"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal execute_transfer_plan output schema: %v", err)
	}
	for _, expected := range []string{"plan_id", "summary", "requested", "upload_requested", "download_requested", "succeeded", "failed", "skipped", "downloads_skipped", "upload_result", "download_result", "error"} {
		if !strings.Contains(string(executeTransferSchema), expected) {
			t.Fatalf("execute_transfer_plan output schema lost %q: %s", expected, executeTransferSchema)
		}
	}
	executeSyncSchema, err := json.Marshal(byName["execute_sync_plan"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal execute_sync_plan output schema: %v", err)
	}
	for _, expected := range []string{"plan_id", "summary", "planned_items", "processed", "succeeded", "skipped", "failed", "blocked", "uploaded_files", "created_remote_directories", "downloaded_files", "created_local_directories", "replaced_remote", "replaced_local", "deleted_remote", "deleted_local", "destructive_actions", "jobs", "file_transfer_slots", "preflight_checked", "preflight_passed", "journal_persisted", "journal_resumed", "journal_completed_before", "journal_version", "journal_state", "journal_status", "items", "relative_path", "action", "status", "recovery_required", "error_code", "error"} {
		if !strings.Contains(string(executeSyncSchema), expected) {
			t.Fatalf("execute_sync_plan output schema lost %q: %s", expected, executeSyncSchema)
		}
	}
	validatePlanSchema, err := json.Marshal(byName["validate_plan"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal validate_plan output schema: %v", err)
	}
	for _, expected := range []string{"valid", "canonical", "plan_id", "plan_version", "kind", "created_from", "safety_class", "operation_count", "dependency_count", "precondition_count", "estimated_bytes", "error_code", "error"} {
		if !strings.Contains(string(validatePlanSchema), expected) {
			t.Fatalf("validate_plan output schema lost %q: %s", expected, validatePlanSchema)
		}
	}
	compareSchema, err := json.Marshal(byName["compare_directories"].OutputSchema)
	if err != nil {
		t.Fatalf("marshal compare_directories output schema: %v", err)
	}
	for _, expected := range []string{"only_left", "only_right", "type_changed", "metadata_changed", "unverified_left", "unverified_right", "absence_comparison_complete"} {
		if !strings.Contains(string(compareSchema), expected) {
			t.Fatalf("compare_directories output schema lost %q: %s", expected, compareSchema)
		}
	}
	assertSchemaOmitsKeys(t, byName["list_offline_pages"].OutputSchema, "url")
	assertSchemaOmitsKeys(t, byName["listOfflineTasks"].OutputSchema, "url")

	for _, toolName := range []string{"upload_from_urls", "upload_from_local_files"} {
		tool := byName[toolName]
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema for dry-run placement: %v", toolName, err)
		}
		if count := strings.Count(string(schema), `"dry_run"`); count != 1 {
			t.Fatalf("%s schema exposes dry_run %d times, want exactly one batch-level flag: %s", toolName, count, schema)
		}
	}

	for _, toolName := range []string{"delete", "move", "rename", "rename_many", "cleanRecycleBin", "deleteOfflineTasks", "clearOfflineTasks"} {
		annotations := byName[toolName].Annotations
		if annotations == nil || annotations.ReadOnlyHint || annotations.DestructiveHint == nil || !*annotations.DestructiveHint {
			t.Fatalf("%s annotations do not declare destructive mutation: %#v", toolName, annotations)
		}
	}
	for _, toolName := range []string{"mkdir", "mkdir_many", "copy", "revertRecycleBin", "addOfflineTaskURIs"} {
		annotations := byName[toolName].Annotations
		if annotations == nil || annotations.ReadOnlyHint || annotations.DestructiveHint == nil || *annotations.DestructiveHint {
			t.Fatalf("%s annotations do not declare additive mutation: %#v", toolName, annotations)
		}
	}
	for _, toolName := range []string{"get_app_versions", "stat", "stat_many", "compare_directories", "plan_sync", "plan_transfer", "revalidate_sync_plan", "revalidate_transfer_plan", "validate_plan", "listDirectory", "list_directories", "list_tree", "resolve_paths", "inspect_paths", "summarize_usage", "listRecycleBin", "list_recycle_pages", "listOfflineTasks", "list_offline_pages", "search_many", "get_share_snaps"} {
		annotations := byName[toolName].Annotations
		if annotations == nil || !annotations.ReadOnlyHint {
			t.Fatalf("%s annotations do not declare read-only behavior: %#v", toolName, annotations)
		}
	}

	batchTool := byName["download_files"]
	if batchTool == nil {
		t.Fatal("download_files is missing when local file capability is enabled")
	}
	batchInput, err := json.Marshal(batchTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal download_files input schema: %v", err)
	}
	for _, expected := range []string{"files", "pick_code", "local_path", "expect_plan_id", "max_checksum_bytes"} {
		if !strings.Contains(string(batchInput), expected) {
			t.Fatalf("download_files input schema lost %q: %s", expected, batchInput)
		}
	}
	if batchTool.OutputSchema == nil {
		t.Fatal("download_files is missing typed output schema")
	}
	assertSchemaOmitsKeys(t, batchTool.OutputSchema, "signed", "cookie", "header", "pick_code", "url")

	singleDownloadTool := byName["download_file"]
	if singleDownloadTool == nil {
		t.Fatal("download_file is missing when local file capability is enabled")
	}
	singleDownloadInput, err := json.Marshal(singleDownloadTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal download_file input schema: %v", err)
	}
	for _, expected := range []string{"pick_code", "local_path", "user_agent", "expect_plan_id", "max_checksum_bytes"} {
		if !strings.Contains(string(singleDownloadInput), expected) {
			t.Fatalf("download_file input schema lost %q: %s", expected, singleDownloadInput)
		}
	}

	planTransferInput, err := json.Marshal(byName["plan_transfer"].InputSchema)
	if err != nil {
		t.Fatalf("marshal plan_transfer input schema for gate placement: %v", err)
	}
	if count := strings.Count(string(planTransferInput), `"max_checksum_bytes":{`); count != 1 {
		t.Fatalf("plan_transfer schema exposes max_checksum_bytes %d times, want exactly one top-level field: %s", count, planTransferInput)
	}
	if strings.Contains(string(planTransferInput), `"expect_plan_id":{`) {
		t.Fatalf("plan_transfer download items unexpectedly expose execution gate: %s", planTransferInput)
	}
	for toolName, encoded := range map[string][]byte{
		"download_files":           batchInput,
		"revalidate_transfer_plan": mustMarshalMCPToolInputSchema(t, byName["revalidate_transfer_plan"]),
		"execute_transfer_plan":    mustMarshalMCPToolInputSchema(t, byName["execute_transfer_plan"]),
	} {
		for _, field := range []string{"expect_plan_id", "max_checksum_bytes"} {
			if count := strings.Count(string(encoded), `"`+field+`":{`); count != 1 {
				t.Fatalf("%s schema exposes %s %d times, want exactly one top-level field: %s", toolName, field, count, encoded)
			}
		}
	}

	shareBatchTool := byName["download_share_files"]
	if shareBatchTool == nil {
		t.Fatal("download_share_files is missing when local file capability is enabled")
	}
	shareBatchInput, err := json.Marshal(shareBatchTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal download_share_files input schema: %v", err)
	}
	for _, expected := range []string{"share_code", "receive_code", "files", "file_id", "local_path"} {
		if !strings.Contains(string(shareBatchInput), expected) {
			t.Fatalf("download_share_files input schema lost %q: %s", expected, shareBatchInput)
		}
	}
	if shareBatchTool.OutputSchema == nil {
		t.Fatal("download_share_files is missing typed output schema")
	}
	assertSchemaOmitsKeys(t, shareBatchTool.OutputSchema, "receive_code", "share_code", "signed", "cookie", "header", "file_id", "url")

	shareTool := byName["download_share_file"]
	if shareTool == nil {
		t.Fatal("download_share_file is missing from default MCP surface")
	}
	shareSchema, err := json.Marshal(shareTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal download_share_file input schema: %v", err)
	}
	shareText := string(shareSchema)
	for _, expected := range []string{"share_code", "receive_code", "file_id", "local_path"} {
		if !strings.Contains(shareText, expected) {
			t.Fatalf("download_share_file input schema lost %q: %s", expected, shareText)
		}
	}
	outputSchema, err := json.Marshal(shareTool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal download_share_file output schema: %v", err)
	}
	outputText := string(outputSchema)
	for _, secretField := range []string{"receive_code", "share_code", "signed", "cookie", "header"} {
		if strings.Contains(strings.ToLower(outputText), secretField) {
			t.Fatalf("download_share_file output schema exposes secret-bearing field %q: %s", secretField, outputText)
		}
	}
}

func TestAllRegisteredToolsExposeTypedOutputSchemas(t *testing.T) {
	tools := registeredTools(t, true, true, true)
	byName := make(map[string]*mcp.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
		if tool.OutputSchema == nil {
			t.Fatalf("registered MCP tool %s has no typed output schema", tool.Name)
		}
	}

	for _, name := range []string{"upload_from_url", "upload_from_urls"} {
		assertSchemaOmitsKeys(t, byName[name].OutputSchema, "source_url", "url", "local_path", "sha1", "network", "endpoint", "cookie", "header")
	}
	for _, name := range []string{"upload_from_local", "upload_from_local_files"} {
		assertSchemaOmitsKeys(t, byName[name].OutputSchema, "local_path", "source_url", "url", "sha1", "network", "endpoint", "cookie", "header")
	}
	for _, name := range []string{"download_file", "download_files"} {
		assertSchemaOmitsKeys(t, byName[name].OutputSchema, "pick_code", "local_path", "url", "signed", "cookie", "header")
	}
	for _, name := range []string{"download_share_file", "download_share_files"} {
		assertSchemaOmitsKeys(t, byName[name].OutputSchema, "receive_code", "share_code", "file_id", "local_path", "url", "signed", "cookie", "header")
	}
	assertSchemaOmitsKeys(t, byName["plan_sync"].OutputSchema, "local_path", "sha1", "pick_code", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["revalidate_sync_plan"].OutputSchema, "local_path", "remote_path", "snapshot_id", "sha1", "pick_code", "source_ref", "target_ref", "ref", "expected", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["plan_transfer"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["revalidate_transfer_plan"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header", "source_ref", "target_ref", "ref", "expected")
	assertSchemaOmitsKeys(t, byName["execute_transfer_plan"].OutputSchema, "local_path", "sha1", "pick_code", "user_agent", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["execute_sync_plan"].OutputSchema, "local_path", "remote_path", "remote_id", "snapshot_id", "sha1", "local_sha1", "remote_sha1", "pick_code", "source_ref", "target_ref", "ref", "expected", "url", "signed", "cookie", "header")
	assertSchemaOmitsKeys(t, byName["validate_plan"].OutputSchema, "source_ref", "target_ref", "ref", "expected")
	for _, name := range []string{"revertRecycleBin", "cleanRecycleBin"} {
		assertSchemaOmitsKeys(t, byName[name].OutputSchema, "password")
	}
	assertSchemaOmitsKeys(t, byName["addOfflineTaskURIs"].OutputSchema, "uris", "url")

	// get_download_info is the intentionally sensitive exception: it is
	// capability-gated precisely because its typed/text output contains URL.
	encoded, err := json.Marshal(byName["get_download_info"].OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"url"`) {
		t.Fatalf("sensitive get_download_info output schema lost url: %s", encoded)
	}
}

func TestRegisteredToolAnnotationsDescribeEffects(t *testing.T) {
	tools := registeredTools(t, true, true, true)
	byName := make(map[string]*mcp.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
		if tool.Annotations == nil {
			t.Fatalf("tool %s has no behavior annotations", tool.Name)
		}
		if tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
			t.Fatalf("tool %s must declare open-world interaction with 115/external services: %#v", tool.Name, tool.Annotations)
		}
	}

	readOnly := []string{
		"getAccountInfo",
		"get_app_versions",
		"getShareSnap",
		"get_share_snaps",
		"compare_directories",
		"diagnose_sync_journal_aliases",
		"diagnose_sync_recovery",
		"inspect_paths",
		"inspect_sync_journal",
		"list_sync_executions",
		"list_sync_journal_trash",
		"listDirectory",
		"list_directories",
		"list_tree",
		"resolve_paths",
		"summarize_usage",
		"listOfflineTasks",
		"list_offline_pages",
		"listRecycleBin",
		"list_recycle_pages",
		"plan_sync",
		"plan_sync_journal_cleanup",
		"plan_sync_journal_alias_repair",
		"plan_transfer",
		"revalidate_sync_plan",
		"revalidate_transfer_plan",
		"validate_plan",
		"search",
		"search_many",
		"stat",
		"stat_many",
		"get_download_info",
	}
	for _, toolName := range readOnly {
		tool := byName[toolName]
		if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
			t.Fatalf("%s must be annotated read-only and idempotent: %#v", toolName, tool)
		}
	}

	additiveMutations := []string{
		"mkdir",
		"mkdir_many",
		"copy",
		"revertRecycleBin",
		"addOfflineTaskURIs",
		"upload_from_url",
		"upload_from_urls",
		"upload_from_local",
		"upload_from_local_files",
	}
	for _, toolName := range additiveMutations {
		tool := byName[toolName]
		if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatalf("%s must be annotated as non-destructive mutation: %#v", toolName, tool)
		}
	}

	destructive := []string{
		"delete",
		"rename",
		"rename_many",
		"move",
		"cleanRecycleBin",
		"deleteOfflineTasks",
		"clearOfflineTasks",
		"download_file",
		"download_files",
		"download_share_file",
		"download_share_files",
		"execute_sync_journal_cleanup",
		"execute_sync_journal_alias_repair",
		"execute_sync_plan",
		"execute_transfer_plan",
		"reconcile_sync_journal_alias",
		"reconcile_sync_recovery",
		"restore_sync_journal",
	}
	for _, toolName := range destructive {
		tool := byName[toolName]
		if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Fatalf("%s must be annotated destructive: %#v", toolName, tool)
		}
	}

	classified := len(readOnly) + len(additiveMutations) + len(destructive)
	if classified != len(tools) {
		t.Fatalf("annotation classification covers %d tools, registered surface has %d", classified, len(tools))
	}
}

func TestClearOfflineTasksWireRejectsExplicitZeroConflict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s := NewServer().WithDestructiveTools(true)
	s.registerTools()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP server: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "clear-wire-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "clearOfflineTasks",
		Arguments: map[string]any{
			"scope":      "all",
			"clear_flag": 0,
		},
	})
	if err != nil {
		t.Fatalf("call clearOfflineTasks: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("explicit zero conflict did not fail closed: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "conflicts with clear_flag 0") {
		t.Fatalf("unexpected explicit-zero conflict result: %#v", result.Content[0])
	}
}

func registeredTools(t *testing.T, allowDestructive, allowSensitive, localFiles bool) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s := NewServer().WithDestructiveTools(allowDestructive).WithSensitiveTools(allowSensitive)
	if localFiles {
		s.WithLocalRoot(t.TempDir())
	}
	s.registerTools()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP server: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "tool-surface-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP client: %v", err)
	}
	defer clientSession.Close()

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list MCP tools: %v", err)
	}
	return result.Tools
}

func registeredToolNames(t *testing.T, allowDestructive, allowSensitive, localFiles bool) []string {
	t.Helper()
	tools := registeredTools(t, allowDestructive, allowSensitive, localFiles)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func assertToolNamesEqual(t *testing.T, got, want []string) {
	t.Helper()
	got = append([]string{}, got...)
	want = append([]string{}, want...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("tool count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool surface mismatch\ngot:  %v\nwant: %v", got, want)
		}
	}
}
