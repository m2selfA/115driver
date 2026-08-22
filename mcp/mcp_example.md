# 115driver MCP Server Example

This document provides examples of how to use the 115driver MCP server.

## Starting the Server

To start the MCP server, run:

```bash
go build -o 115driver-mcp-server ./cmd/115driver-mcp-server
```

Then run the server with your 115 cookies:

```bash
./115driver-mcp-server --cookie="UID=your_uid;CID=your_cid;SEID=your_seid"
```

The server will listen on stdin/stdout for MCP requests.

Local file access is disabled by default, and local file tools are omitted from
`tools/list` until a local root is configured. To register `download_file` and
`download_share_file`, and to register `upload_from_local` when destructive tools
are enabled, start the server with an existing directory as local root. The root
is canonicalized before authentication; target paths and existing parent
directories are resolved before each boundary check so symlinks cannot point
local file tools outside `--local-root`:

```bash
./115driver-mcp-server --cookie="UID=your_uid;CID=your_cid;SEID=your_seid" --local-root="/safe/path"
```

MCP HTTP transfers default to a 2 hour total timeout. Override it with
`--download-timeout`, or use `--download-timeout=0` to disable the total timeout:

```bash
./115driver-mcp-server --cookie="UID=your_uid;CID=your_cid;SEID=your_seid" --download-timeout=6h
```

All MCP download tools have no size limit by default. `--download-max-bytes`
is enforced per file for both single-file and batch downloads. `upload_from_url`
defaults to a 2 GiB limit because it buffers through a local temp file. Use
`--download-max-bytes` or `--url-upload-max-bytes` to set limits; `0` disables
the corresponding size limit.
For SSRF protection, `upload_from_url` rejects non-HTTP(S) URLs, redirects to
unsafe hosts, and hostnames that resolve to loopback/private/link-local IPs.
When a hostname resolves to multiple safe IPs, MCP HTTP transfers try the later
addresses if earlier addresses are unreachable.

Tools that mutate 115 cloud state are not registered by default. Start the
server with `--allow-destructive-tools` to enable directory creation, file
rename/move/copy/delete, cloud uploads, recycle-bin mutations, and offline task
mutations. Mutation tools that expose `dry_run` perform their complete static/read-only
preflight but do not submit the write request:

```bash
./115driver-mcp-server --cookie="UID=your_uid;CID=your_cid;SEID=your_seid" --allow-destructive-tools
```

The signed-URL inspection tool is also disabled by default. Enable it separately
only when the MCP client really needs a short-lived 115 CDN URL:

```bash
./115driver-mcp-server --cookie="UID=your_uid;CID=your_cid;SEID=your_seid" --allow-sensitive-tools
```

## Available Tools

Every registered tool publishes a typed MCP `outputSchema` and returns `structuredContent`; JSON `TextContent` is retained for compatibility with existing clients. Credential-bearing/control-plane fields are deliberately omitted from structured output as well as text output.

### Account Tools

1. `getAccountInfo`: Get current account, storage space, and login device info
   - Parameters: none
   - Returns: `user`, `space.total`, `space.remain`, `space.used`, and `login_devices`; raw `imei_info` is intentionally omitted from MCP output

2. `get_app_versions`: Get currently advertised 115 application versions from the remote version service
   - Parameters: none
   - Returns: a stable sorted `versions` array containing `app` and `version`, plus `count`

### Directory Tools

`resolve_paths`, `inspect_paths`, `list_tree`, `compare_directories`, `summarize_usage`, and `plan_sync` use a request-scoped read snapshot for identical directory pages. The snapshot never crosses tool calls, retains at most 50,000 file entries, and reserves at most 100,000 new `ListPage` entries per call; cache hits and concurrent waiters do not consume the remote-read budget again. Once the read budget is exhausted, a new page fails before another network request is issued.

1. `listDirectory`: List files and directories in a specific directory. Both paged and unpaged reads explicitly disable 115 `record_open_time`.
   - Parameters:
     - `dir_id` (string): Directory ID to list, default is root directory (0)
     - `offset` (int64): Offset for pagination, default is 0
     - `limit` (int64): Number of items to return. Omit or set to 0 to return all items; positive limits above 500 are rejected before network access

2. `list_directories`: List up to 256 independently paginated directory pages in one read-only call. Every page fixes `record_open_time=0`; the aggregate effective page budget is 5000 entries.
   - Parameters: `directories`, an array of `dir_id`, `offset`, and `limit` items
   - Defaults: blank `dir_id` means root `0`; per-item `limit` defaults to 100, and values above 500 are rejected before network access
   - Pagination: because the directory page API does not expose a stable total to this layer, `next_offset` is a conservative candidate emitted when a page fills its requested limit

3. `resolve_paths`: Resolve up to 256 remote paths to stable 115 object IDs without mutation
   - Parameters: `paths` (array of remote paths); blank and duplicate logical paths are rejected
   - Returns each path with `file_id`, `is_directory`, success/error, preserving input order

4. `inspect_paths`: Resolve and inspect up to 128 remote paths in one bounded read-only call
   - Parameters: `paths` (array of remote paths); uses the same blank/duplicate preflight as `resolve_paths`
   - Returns resolution status, stable object ID/type, `metadata_complete`, and a safe metadata entry without thumbnail or signed URLs
   - A path can be `resolved=true` but `success=false` when the follow-up metadata read fails, allowing clients to distinguish resolution from metadata failures without parsing error text

5. `list_tree`: Recursively list descendants for up to 32 directory roots using read-only paged traversal
   - Parameters: `paths`, optional `max_depth` (`0` = unlimited), optional `max_nodes`
   - Node budget: default 1000, maximum 5000 across all roots
   - Partial results explicitly set `complete`, `depth_limited`, and/or `node_limited`; unsafe child names and repeated directory IDs fail closed

6. `summarize_usage`: Compute file bytes and descendant counts for up to 64 remote paths
   - Parameters: `paths`, optional `max_depth` (`0` = unlimited), optional `max_nodes`
   - Node budget: default 10000, maximum 50000 across all paths
   - Returns explicit bounded partial summaries rather than silently treating depth/node-limited results as complete

7. `compare_directories`: Compare two remote directory trees using bounded read-only traversal
   - Parameters: `left_path`, `right_path`, optional `max_depth` (`0` = unlimited), optional `max_nodes` (default 1000, maximum 5000), and optional `include_unchanged`
   - The aggregate node budget starts with a fair split, transfers unused left budget to the right, and can reuse unused right budget for one cached left-side retry
   - Returns `only_left`, `only_right`, `type_changed`, and `metadata_changed`; unchanged pairs are returned only when requested, while `unchanged_count` is always present
   - If node truncation or a traversal failure makes an absence claim unsafe, unmatched entries are placed in `unverified_left`/`unverified_right` instead. A depth bound still permits absence claims within the fully enumerated visible depth
   - Metadata equality intentionally uses file size, SHA1, and star state; object IDs, pick codes, and timestamps are not equality keys

8. `plan_sync`: Build a bounded, read-only local/remote sync plan. Requires `--local-root`; it does not require `--allow-destructive-tools` because it never mutates either side.
   - Parameters: `local_path`, `remote_path`, optional `direction` (`both`, `upload`, or `download`), optional `conflict_policy` (`error`, `prefer-local`, or `prefer-remote`), optional `delete`, optional `max_nodes` (default 1000, maximum 5000), and optional `max_checksum_bytes` (default 4 GiB, maximum 64 GiB)
   - Uses the same conservative sync classifier and digest semantics as CLI `sync`; `.115driver-*.session.json` and matching session-parts state are excluded from the local tree
   - Planning fails instead of returning a partial plan when the combined local/remote node budget is exhausted or required local hashing would exceed its checksum budget
   - Returns a content-addressed `MCPPlan v1`, an opaque hidden `snapshot_id`, safe relative-path decisions, action/count/byte estimates, and destructive-action classification; it deliberately omits local absolute paths, SHA1 values, pick codes, signed URLs, cookies, and headers

9. `revalidate_sync_plan`: Recheck a reviewed `plan_sync` against current local and remote state. Requires `--local-root` and remains read-only.
   - Parameters: the same `local_path`, `remote_path`, `direction`, `conflict_policy`, `delete`, `max_nodes`, and `max_checksum_bytes` used for planning, plus required `expect_plan_id`
   - Reruns the complete bounded scan/classifier instead of trusting the old snapshot. `gate_satisfied=true` only when the fresh plan ID equals `expect_plan_id` and the fresh plan is still `ready=true`
   - If current state changed, returns `matches=false`, `error_code=plan_changed`, and a generic re-plan message; it deliberately withholds the new plan ID, new snapshot ID, and changed actions so callers must invoke `plan_sync` again for review
   - A matching but unresolved-conflict plan reports `matches=true`, `ready=false`, and `gate_satisfied=false`. Revalidation does not execute sync and does not grant destructive capability

`inspect_sync_journal`: Inspect a persisted sync execution journal by reviewed `plan_id`. Requires `--local-root` and is read-only; it does not require `--allow-destructive-tools`.
   - Accepts `plan_id` only in `sha256:<64hex>` form and lazily binds the journal store to the authenticated account through the read-only user-info endpoint
   - Returns schema/version/state/status, recovery/reconcile flags, safe aggregate run/item counters, and per-item relative path/action/state/phase/attempt metadata
   - Deliberately omits stored local/remote absolute paths, remote object IDs, SHA1 values, postconditions, and raw journal error text; missing, legacy, or unreadable journals return stable sanitized error codes

`diagnose_sync_recovery`: Read and classify stable evidence for any reconciliation-gated sync item by reviewed `plan_id`. Requires `--local-root` and remains read-only.
   - Refuses diagnosis while the journal lock is in use; it never edits the journal, local files, or 115 objects
   - Interrupted destructive items use the shared `completed` / `retry-full` / `winner-only` / `ambiguous` classifier. Ordinary `upload`/`download` items stuck at `mutation-done` are verified against the immutable reviewed content snapshot and return `completed`, `pending-observation`, or `ambiguous`; they are never blindly replayed
   - Stable hidden evidence is bound into an opaque content-addressed `diagnosis_id` (`sha256:<64hex>`). `resolvable=true` requires no ambiguity, errors, or pending observations and still does not authorize execution
   - Local evidence hashing is bounded by a fixed 64 GiB per diagnosis/reconciliation pass. Planned local digest work is budgeted before evidence I/O and actual file sizes consume the runtime budget again before hashing; output exposes only aggregate `checksum_budget_bytes` and `checksummed_bytes`
   - Output contains only reviewed plan ID, safe journal state/status, relative path/action/kind/state/phase, and aggregate evidence counts. Local/remote absolute identities, object IDs, SHA1/postconditions, and raw stored errors are omitted from both structured and text content

`reconcile_sync_recovery`: Apply an explicitly reviewed recovery diagnosis to persistent journal control state. Requires both `--allow-destructive-tools` and `--local-root`; it never mutates local files or 115 objects.
   - Parameters: reviewed `plan_id` plus required `expect_diagnosis_id` returned by `diagnose_sync_recovery`
   - Reacquires the shared journal lock and re-collects the evidence before changing journal state. The content-addressed diagnosis must still match exactly; changed evidence returns `diagnosis_changed` without returning the newly computed token, forcing a fresh diagnosis review, while ambiguous evidence remains blocked
   - A verified non-destructive `mutation-done` item is marked succeeded with its observed postcondition without replaying upload/download. `pending-observation` returns `postcondition_pending` and leaves the journal unchanged until a later fresh diagnosis
   - A successful result may report `resume_candidate=true`, but that only clears the journal control gate. `execute_sync_plan` still performs the complete expected-tree, completed-postcondition, budget, and residual preflight before any user-content mutation
   - Structured and text output omit internal journal IDs/paths, absolute local/remote identities, object IDs, hashes, postconditions, and raw stored errors

`list_sync_executions`: List recent persistent sync executions for the current profile/account. Requires `--local-root` and is read-only.
   - Optional `limit` defaults to 50 and is capped at 128; optional `status` filters by effective journal status. The control plane scans at most 4096 journal files and fails closed if that bound is exceeded
   - Reconstructs the original reviewed MCP `plan_sync` plan ID from each current-v2 raw sync plan, so callers never need or receive the internal journal plan ID
   - Returns only safe state/status/count/timestamp/run/lock metadata. Legacy readable journals are omitted from items and counted in `migration_required`; journal paths, profile/account scope, local/remote roots, object IDs, postconditions, raw errors, and hashes are never returned

`diagnose_sync_journal_aliases`: Audit private reviewed-plan alias lifecycle. Requires `--local-root` and is read-only.
   - Optional `limit` defaults to 50 and is capped at 128; the underlying alias and trash scans each fail closed at the 4096-entry safety ceiling
   - CLI doctor and MCP use one shared `internal/syncjournal` current/trash/account lifecycle classifier. MCP additionally verifies that a live target reconstructs to the same content-addressed MCP `plan_sync` ID as its alias
   - Classifies aliases as `live`, `orphan`, `soft-deleted-shadow`, `identity-mismatch`, or `invalid-target`. A missing current target is cross-checked through one bounded Session Store trash pass before it can be called orphan. Only raw-plan IDs relevant to a missing alias are decoded; invalid/incomplete matching evidence fails closed, while unrelated damaged trash only consumes the scan budget and does not block another alias's proof
   - Only a strictly proven `orphan` item receives `repair_id`. This opaque content-addressed token binds the hidden raw plan ID, exact profile/account binding, alias creation/update timestamps, and orphan status. Non-orphan states never receive a repair token
   - The tool itself never repairs or deletes aliases. Output contains reviewed MCP plan IDs, safe lifecycle status/counts, lock state, optional orphan `repair_id`, and sanitized error codes only; raw journal plan IDs, journal/trash paths, account/profile bindings, roots, object IDs, hashes, postconditions, and private alias sidecars are omitted

`reconcile_sync_journal_alias`: Remove one exactly reviewed orphan alias. Requires both `--allow-destructive-tools` and `--local-root`; it changes only private control-plane alias metadata and never local/115 user content.
   - Parameters: reviewed orphan `plan_id` plus required `expect_repair_id` returned by `diagnose_sync_journal_aliases`
   - Reruns the complete bounded shared alias diagnosis first. Only a still-proven `orphan` may proceed; `soft-deleted-shadow` returns `journal_trashed` and must use `restore_sync_journal`, while `identity-mismatch` returns `journal_alias_conflict` and is never auto-repaired
   - The recomputed repair token must exactly equal `expect_repair_id`. `repair_changed` does not return the newly computed token, forcing a new diagnosis/review rather than silently authorizing changed state
   - Removal then acquires the raw-journal lock before the alias lock, requires every persisted alias identity/binding/timestamp field to still match the reviewed snapshot, and re-proves both canonical-current absence and matching-trash absence before deleting the alias
   - Structured/text output contains only the reviewed `plan_id`, reviewed `repair_id`, repaired/status flags, and sanitized errors. Raw journal IDs, journal/trash paths, profile/account bindings, roots, object IDs, hashes, and sidecars remain hidden
   - The local CLI exposes the same reviewed token protocol as `sync journal aliases diagnose` → `sync journal aliases reconcile <review_id> --expect-repair-id <repair_id>`. Both frontends derive the token with `internal/syncjournal.ReviewAliasRepairID`; CLI remains offline/profile-scoped while MCP additionally verifies live aliases against the MCP `plan_sync` identity

`plan_sync_journal_alias_repair`: Preview a bounded orphan-only batch repair. Requires `--local-root` and is read-only.
   - Optional `limit` defaults to 50 and is capped at 128. The tool reruns the complete bounded shared lifecycle diagnosis and includes only strictly proven `orphan` aliases; live, soft-deleted, identity-mismatched, and invalid aliases are never selected
   - Returns up to `limit` candidates with reviewed `plan_id` plus individual opaque `repair_id`, together with `repair_set_id`
   - `repair_set_id` binds the requested limit and the **complete currently diagnosed orphan set**, not only the selected prefix. Changing an unselected orphan therefore invalidates an old batch review just like changing a selected one
   - Planning never removes aliases and output omits raw plan IDs, profile/account binding, journal/trash paths, roots, hashes, postconditions, and private sidecar evidence

`execute_sync_journal_alias_repair`: Apply one exactly reviewed bounded orphan batch. Requires both `--allow-destructive-tools` and `--local-root`.
   - Parameters: the same optional `limit` plus required `expect_repair_set_id` returned by `plan_sync_journal_alias_repair`
   - Before the first removal it rebuilds the complete orphan diagnosis and exact set token. `repair_changed` performs zero mutations, reports `Requested=0` with `items=[]`, and does not return a replacement token/candidate set; run a fresh `plan_sync_journal_alias_repair` and review changed state before continuing
   - After the batch gate matches, the shared exact-batch primitive acquires every distinct raw-plan lock in sorted raw-ID order and then every alias lock in sorted reviewed-ID order. While all locks are held it re-reads every exact persisted alias snapshot, proves every current target absent, and completes one bounded matching-trash check for the whole selected set **before removing the first alias**
   - Snapshot drift, a late current/trash target, or lock contention therefore leaves every selected item `unchanged`. If a later alias-file removal itself fails, already removed aliases are restored from the reviewed snapshots before locks are released, so successful rollback also leaves the whole set unchanged
   - Catchable removal failure is therefore all-or-rollback under the held lock set. Abrupt process termination is intentionally **not power-loss atomic**: aliases already removed before process death may stay absent. Recovery is fail-safe by **monotonic convergence**: the next bounded diagnosis sees only remaining orphans, the old set token becomes stale, and no further alias can be removed until a fresh plan is explicitly reviewed
   - Only alias rollback failure is control-state ambiguous: output sets `recovery_required=true`, marks item state `unknown`, and may set `partial=true`. Run a fresh `diagnose_sync_journal_aliases` before any further repair rather than assuming which alias survived
   - The tool never auto-repairs `soft-deleted-shadow`, `identity-mismatch`, or `invalid-target` states and never uploads/downloads/deletes local or 115 user content. Structured/text output contains only reviewed IDs/tokens, safe counts/status, and sanitized errors
   - Offline CLI administration uses the same set token and exact-batch primitive as `sync journal aliases plan [--limit N]` → `sync journal aliases reconcile-batch --limit N --expect-repair-set-id <repair_set_id>`. Profile-only CLI repair can span persisted accounts in that profile; authenticated MCP remains exact-account scoped and fails closed rather than silently skipping a foreign-account alias binding

`plan_sync_journal_cleanup`: Preview retention-GC candidates without changing persistent state. Requires `--local-root` and remains read-only.
   - Parameters: optional `older_than_hours` (`0` uses `[transfer.sessions].retention`, then the historical 30-day fallback; maximum 87600) and optional `limit` (default 50, maximum 128)
   - Uses the same shared eligibility policy as CLI sync journal GC: only old `completed`/`failed` current-v2 journals are eligible; active, locked, recovery-required, reconcile-required, and fresh journals fail closed and remain untouched
   - If the CLI bulk-migration marker exists, the whole preview returns `journal_migration_in_progress` instead of guessing which migration evidence may be collected
   - Returns reviewed MCP `plan_sync` IDs plus safe age/run counters and a content-addressed `cleanup_id` bound to retention, limit, candidate state, and update timestamps. Raw internal plan IDs, journal paths, roots, object IDs, hashes, postconditions, and raw errors are omitted
   - The preview itself never trashes or deletes journals; a later mutating cleanup must re-plan and match the reviewed `cleanup_id`

`execute_sync_journal_cleanup`: Move exactly the reviewed cleanup candidate set into Session Store trash. Requires both `--allow-destructive-tools` and `--local-root`.
   - Parameters: the same optional `older_than_hours` and `limit` used for planning, plus required `expect_cleanup_id`
   - Acquires the shared sync-journal GC lock and CLI bulk-migration lock, rebuilds the preview, and performs zero journal moves when the content-addressed cleanup ID changed. The replacement cleanup ID/candidate set is not returned; run `plan_sync_journal_cleanup` again
   - Each selected journal is then locked and rechecked for exact state, update timestamp, age, and safe GC eligibility before it is renamed into the common Session Store `trash/` directory. The complete current reviewed-plan alias set plus the reviewed ID that authorized cleanup is collected, deterministically alias-locked, written to a private `review-aliases.json` sidecar in trash, and removed from the live binding namespace; sidecar/alias/stamp failures attempt a journal+alias rollback
   - If a candidate changes after the batch review, execution stops at that candidate; already moved journals stay in trash and remaining candidates are explicitly `skipped`, so partial execution is machine-visible rather than presented as atomic
   - Output contains reviewed MCP plan IDs and safe per-item status only. Raw journal/trash paths, internal plan IDs, local/remote roots, object IDs, hashes, postconditions, and raw stored errors remain hidden
   - With `[transfer.sessions].auto_gc=true`, persistent sync/cleanup execution also best-effort invokes the existing SessionStore opportunistic GC using the shared `gc_interval`, `retention`, and `trash_retention`: expired ordinary transfer sessions and common trash (including previously trashed sync journals) may be purged, but current sync journals are never auto-selected outside the reviewed `cleanup_id` flow

`list_sync_journal_trash`: List recoverable current-v2 sync journals in the shared Session Store trash. Requires `--local-root` and remains read-only.
   - Optional `limit` defaults to 50 and is capped at 128; scanning itself is bounded by the same 4096-journal safety ceiling
   - Only account/profile-bound `sync-journal-*` trash entries with valid current-v2 journals are projected. Foreign-account entries are omitted, while legacy/invalid entries are represented only by aggregate counts
   - Each item exposes the reviewed MCP `plan_sync` ID, safe journal state/run timestamps, `trash_age_ms`, `trash_retention_ms`, `purge_eligible_at`, `purge_eligible`, and a content-addressed `restore_id` bound to the hidden raw journal ID, trash name, journal state/status, journal update time, trash move time, and the complete hidden reviewed-plan alias set. `purge_eligible_at` is only the point at which opportunistic Session Store GC is allowed to purge the entry; it is not a guaranteed deletion schedule. Sidecar tampering invalidates the old restore token even if the trash directory mtime is restored
   - Trash names/paths, internal plan IDs, roots, object IDs, hashes, postconditions, and raw errors are never returned

`restore_sync_journal`: Restore one exactly reviewed trashed sync journal to current-v2 persistent state. Requires both `--allow-destructive-tools` and `--local-root`; it never mutates user files or 115 objects.
   - Parameters: reviewed `plan_id` plus required `expect_restore_id` from `list_sync_journal_trash`
   - Acquires the shared GC/bulk-migration guard, rescans trash, matches the exact restore token, then locks the raw plan ID without creating a lease-backed phantom current directory
   - Restore requires the canonical current journal location to remain absent and the trash journal/update/move/alias-set snapshot to still match the reviewed token. It renames the journal back to current-v2 and recreates/verifies the complete reviewed alias set from the private sidecar; alias failure attempts to roll both aliases and the journal back into trash
   - `current_exists`, `restore_changed`, migration/lock contention, and alias conflicts fail closed without consuming the trash entry. Restored recovery/reconcile-required journals keep those gates and must still follow the normal diagnosis/reconciliation workflow
   - CLI/MCP sync-journal trash movement stamps the destination directory with the actual move time, so shared Session Store `trash_retention` starts at soft-delete time rather than at the journal's much older last activity time
   - CLI current-v2 `sync journal rm/gc` uses the same alias-aware soft-delete primitive, and `sync journal trash list` / `sync journal trash restore <plan_id>` provide a local raw-ID recovery path that also restores every sidecar alias. Readable legacy CLI journals keep the historical raw-trash fallback and are not migrated merely for deletion

10. `execute_sync_plan`: Execute or safely resume a reviewed sync plan through the shared `internal/syncexec` engine. Requires both `--allow-destructive-tools` and `--local-root`.
   - Parameters: the same `local_path`, `remote_path`, `direction`, `conflict_policy`, `delete`, `max_nodes`, and `max_checksum_bytes` used by `plan_sync`; required `expect_plan_id`; optional execution-only `max_delete_roots`, `max_delete_items`, and `max_delete_bytes` destructive-removal budgets (0 = unlimited; they cover mirror deletes plus replacement losers); optional `jobs` (default 1, maximum 16), `continue_on_error`, and `max_errors`
   - Fresh execution keeps the original two full live `plan_sync` gates. If the reviewed plan ID already resolves to a persistent current-v2 journal, MCP locks that exact raw journal, reconstructs its content-addressed MCP plan identity, and requires the alias reviewed ID to match before any resume state change; mismatch returns `journal_alias_conflict`. It then verifies request/root/policy binding, the original destructive budgets, the whole expected tree and every completed postcondition, builds a residual plan, and repeats the journal-state gate immediately before the first residual write. Completed actions are not replayed
   - If a reviewed alias points to a missing current journal, stale-alias self-heal first proves that neither the canonical current journal nor a matching soft-deleted trash journal exists. Matching trash returns `journal_trashed` and requires restore/review rather than falling through to fresh execution; this covers the crash window after current→trash rename but before sidecar/live-alias cleanup completes
   - A changed completed object or tree mismatch returns `journal_state_changed` before mutation. A nonterminal `mutation-done` with no verifiable postcondition returns `journal_reconcile_required`; destructive crossed phases remain recovery-gated
   - Every action then revalidates the concrete object it will touch. Uploads re-hash the local file and pass that exact prepared digest into P10; downloads/deletes verify remote ID/type/size/SHA1, and downloads hash the completed local file against the planned remote SHA1 when available
   - File transfers use one additional transfer slot in this first MCP release even when `jobs>1`; independent directory/metadata/delete actions still use the shared wave scheduler and dependency DAG. `continue_on_error` blocks descendants of a failed item while allowing independent branches to proceed, with optional `max_errors`
   - Destructive directory delete/replace uses the same shared `internal/syncguard` subtree validator as CLI sync: after root snapshot validation, every planned descendant must still match and any unexpected/deleted/changed deep entry stops `Delete`/`RemoveAll` before mutation. Replacement validates the winning side before removing the old directory tree
   - Destructive-removal budgets count collapsed mirror-delete roots and replacement loser roots plus every affected descendant file/directory and removed file byte. They are checked after the reviewed plan matches and again at the pre-write live-state barrier
   - Destructive ambiguity is machine-visible: lost remote mutation responses, potentially partial directory removals, or a failed replacement winner after its loser was removed set `recovery_required=true` and `error_code=recovery_required`. Do not replay the reviewed plan blindly: inspect the journal, diagnose the stable evidence, reconcile only an exact reviewed `diagnosis_id` when it is resolvable, then invoke `execute_sync_plan` again; truly ambiguous evidence remains blocked and requires a new plan/manual review
   - Execution persists the same current-v2 profile/account-scoped sync journal and `sync-locks` protocol as CLI sync under the configured transfer-session store. The journal records attempts/phases and verified postconditions. Concurrent same-plan execution returns `journal_in_use`; an interrupted destructive phase blocks blind replay with `recovery_required`; legacy readable journals return `journal_migration_required` and must be migrated by the CLI. Only safe journal summary fields are returned to MCP; raw journal IDs/paths, absolute paths, remote IDs, and hashes stay hidden
   - Execution is not atomic and does not roll back already completed actions. Output contains only the reviewed plan ID, safe aggregate counts, relative-path item status, and sanitized errors; local/remote absolute paths, remote IDs, SHA1 values, pick codes, signed URLs, cookies, and headers are omitted

11. `mkdir`: Create a new directory. Requires `--allow-destructive-tools`.
   - Parameters:
     - `parent_id` (string): Parent directory ID
     - `name` (string): Name of the new directory
     - `dry_run` (bool): Preview after parent/name preflight without creating the directory

12. `mkdir_many`: Create up to 256 directories after full-batch parent/name and sibling-name preflight. Requires `--allow-destructive-tools`.
   - Parameters:
     - `directories` (array): Items containing `parent_id` and `name`
     - `dry_run` (bool): Preview the complete batch without creating anything
     - `continue_on_error` (bool): Continue later items after a runtime failure; ignored by dry-run

### File Tools

`validate_plan`: Statically validate an `MCPPlan v1` produced by any planner. It is always available on the default read-only surface and performs no local or remote state I/O.
- Checks the content-addressed `plan_id`, supported version, normalized fields/order, operation identities, dependency DAG, precondition ownership, safety classification, and byte-estimate consistency.
- Returns only aggregate validation metadata plus sanitized `error_code`/`error`; source/target refs and opaque precondition values are never reflected in output.
- `valid=true` does **not** establish freshness. Before any future execution path, the original planner inputs must be re-preflighted and the freshly produced `plan_id` must match the reviewed expected ID.

`plan_transfer`: Build a read-only mixed upload/download `MCPPlan v1`. Requires `--local-root` but not `--allow-destructive-tools`, because planning itself never transfers data.
- Parameters: optional `uploads` items with `local_path`, `dir_id`, and optional `file_name`; optional `downloads` items with `pick_code`, `local_path`, and optional `user_agent`; optional `max_checksum_bytes` for all local content snapshots (upload sources plus already-existing download targets; default 4 GiB, maximum 64 GiB). At least one item is required and the aggregate maximum is 256.
- Reuses the exact batch preflight paths used by `upload_from_local_files` and `download_files`, including local-root/symlink containment, duplicate remote upload targets, duplicate download sources/targets, target-directory validation, transfer configuration checks, authenticated download metadata, and download size/strategy limits.
- Before the first content hash, sums every upload source plus every already-existing download target and fails closed when the checksum budget would be exceeded. Both classes bind canonical local identity, size, and SHA-256 content; timestamp-only changes do not alter the plan, while same-size/same-mtime content rewrites do.
- Returns uploads first and downloads second in deterministic input order. Local paths, content digests, and pick codes participate only in opaque SHA-256 snapshot preconditions; they are absent from output together with signed URLs, user agents, cookies, and headers.
- Upload operations are additive. Downloads to absent local targets are additive; downloads to already-existing validated regular files are marked destructive. Unknown download sizes remain unknown rather than being reported as zero.
- `upload_from_local`, `download_file`, `upload_from_local_files`, and `download_files` accept optional `expect_plan_id` execution gates for single-direction plans. They rerun their normal preflight and rebuild the plan from the same prepared source/target state before touching the data path. All gated forms accept `max_checksum_bytes`; `download_file` owns its gate at the tool top level while the shared `DownloadFileArgs` item used by `plan_transfer.downloads[]` and `download_files.files[]` remains gate-free.

`revalidate_transfer_plan`: Recheck a reviewed `plan_transfer` against current local/download metadata and local content state. Requires `--local-root` and remains read-only.
- Parameters: the same optional `uploads`, `downloads`, and `max_checksum_bytes` used during planning, plus required `expect_plan_id`.
- Reruns the complete transfer preflight and local content snapshots but never invokes upload/download data paths.
- A fresh match returns `matches=true` and `gate_satisfied=true` with aggregate safe metadata. A changed plan returns `error_code=plan_changed` without the replacement plan ID or changed operations, forcing an explicit new `plan_transfer` review.
- Output never contains local paths, pick codes, user agents, content digests, signed URLs, cookies, headers, or opaque operation refs.

`execute_transfer_plan`: Execute a reviewed mixed `plan_transfer` request. Requires both `--allow-destructive-tools` and `--local-root`.
- Parameters: the same optional `uploads` and `downloads`, required `expect_plan_id`, and optional `max_checksum_bytes`; at least one item is required and the aggregate maximum is 256.
- Preflights the complete upload and download inputs before mutation, rebuilds the whole `MCPPlan v1`, and starts no transfer unless the rebuilt ID exactly matches `expect_plan_id`.
- Executes uploads first, then downloads. Any upload item failure causes every planned download to be reported as skipped rather than started; completed uploads are not rolled back, so this is explicitly not an atomic transaction.
- Output contains the matched plan ID, aggregate success/failure/skip counts, and safe per-phase results. It never returns local paths, pick codes, user agents, content digests, signed URLs, cookies, or request headers.

1. `delete`: Delete files or directories. Requires `--allow-destructive-tools`.
   - Parameters:
     - `file_ids` (array of strings): IDs of files or directories to delete
     - `dry_run` (bool): Resolve and preview every source without deleting anything

2. `rename`: Rename a file or directory. Requires `--allow-destructive-tools`.
   - Parameters:
     - `file_id` (string): ID of file or directory to rename
     - `new_name` (string): New name for the file or directory
     - `dry_run` (bool): Preview source/name/sibling preflight without renaming

3. `rename_many`: Rename up to 256 objects after full-batch source/name/sibling preflight. Requires `--allow-destructive-tools`.
   - Parameters:
     - `files` (array): Items containing `file_id` and `new_name`
     - `dry_run` (bool): Preview the complete batch without renaming anything
     - `continue_on_error` (bool): Continue later renames after a runtime failure

4. `move`: Move files or directories to another directory after source/target/ancestry preflight. Requires `--allow-destructive-tools`.
   - Parameters:
     - `dir_id` (string): Target directory ID
     - `file_ids` (array of strings): IDs of files or directories to move
     - `dry_run` (bool): Preview without moving; descendant targets are rejected before mutation

5. `copy`: Copy files or directories to another directory after source/target/ancestry preflight. Requires `--allow-destructive-tools`.
   - Parameters:
     - `dir_id` (string): Target directory ID
     - `file_ids` (array of strings): IDs of files or directories to copy
     - `dry_run` (bool): Preview without copying

6. `stat`: Get detailed information about a file or directory
   - Parameters:
     - `file_id` (string): ID of file or directory to get info

7. `stat_many`: Get detailed metadata for up to 256 files or directories in one bounded read-only batch. Blank or duplicate IDs are rejected before network access; lookup failures are reported per item while preserving input order.
   - Parameters:
     - `file_ids` (array of strings): IDs of files or directories to inspect

8. `download_file`: Download a file from 115 cloud storage to a local path.
   Requires `--local-root`.
   - Parameters:
     - `pick_code` (string): Pick code of the file to download
     - `local_path` (string): Local path under `--local-root`
     - `user_agent` (string): Optional User-Agent

9. `download_files`: Download multiple 115 files in one preflighted batch.
   Requires `--local-root`. The complete batch is validated before data transfer;
   with the `file` strategy, all files share one cross-interface scheduler run.
   - Parameters:
     - `files` (array): Items containing `pick_code`, `local_path`, and optional `user_agent`

10. `download_share_file`: Download one file from a 115 share to a local path.
   Requires `--local-root`. The receive code/password, signed CDN URL, cookies,
   and request headers are used internally but are not returned in MCP content.
   - Parameters:
     - `share_code` (string): Share code
     - `receive_code` (string): Share receive code/password
     - `file_id` (string): File ID inside the share
     - `local_path` (string): Local path under `--local-root`
     - `user_agent` (string): Optional User-Agent

11. `download_share_files`: Download multiple file IDs from one 115 share.
   Requires `--local-root`. Share credentials are used internally but are not
   emitted in the batch result. With the `file` strategy, all files share one
   cross-interface scheduler run after full-batch preflight.
   - Parameters:
     - `share_code` (string): Share code
     - `receive_code` (string): Share receive code/password
     - `files` (array): Items containing `file_id` and `local_path`
     - `user_agent` (string): Optional User-Agent for all items

12. `get_download_info`: Get a short-lived signed file download URL and metadata.
   Requires `--allow-sensitive-tools` because the signed URL is returned directly in MCP content.
   - Parameters:
     - `pick_code` (string): Pick code of the file
     - `user_agent` (string): Optional User-Agent

13. `upload_from_url`: Download a URL and upload it to 115 cloud storage.
   Requires `--allow-destructive-tools`.
   - Parameters:
     - `url` (string): HTTP or HTTPS URL to download
     - `dir_id` (string): Target 115 directory ID
     - `file_name` (string): Optional destination file name
     - `dry_run` (bool): Validate URL/name/115 target and return a safe plan without DNS/HTTP fetch or upload

14. `upload_from_urls`: Fetch and upload multiple external URLs in one call.
    Requires `--allow-destructive-tools`. Up to 256 URLs are accepted. All URLs,
    remote names, and unique 115 target directories are validated before the
    first external fetch. Runtime failures are reported per item without echoing
    source URLs or URL credentials.
    - Parameters:
      - `files` (array): Items containing `url`, `dir_id`, and optional `file_name`
      - `dry_run` (bool): Validate the entire batch and return safe destination metadata without fetching any source URL; use this batch-level flag rather than item-level `dry_run`

15. `upload_from_local`: Upload a local file to 115 cloud storage.
    Requires `--allow-destructive-tools` and `--local-root`.
    - Parameters:
      - `local_path` (string): Local file path under `--local-root`
      - `dir_id` (string): Target 115 directory ID
      - `file_name` (string): Optional destination file name
      - `dry_run` (bool): Validate the local source/name/115 target and return a safe plan without invoking P10

16. `upload_from_local_files`: Upload multiple local files in one preflighted call.
    Requires `--allow-destructive-tools` and `--local-root`. Each item may target
    a different 115 directory. All local sources and remote names are validated
    before the first upload; runtime failures are reported per item and later
    items continue through the same P10 multi-interface uploader.
    - Parameters:
      - `files` (array): Items containing `local_path`, `dir_id`, and optional `file_name`
      - `dry_run` (bool): Validate the entire batch and return file name/size/destination metadata without uploading; source paths are not returned

### Recycle Bin Tools

1. `listRecycleBin`: List items in the recycle bin
   - Parameters:
     - `offset` (int): Offset for pagination, default is 0
     - `limit` (int): Number of items to return, default is 40, maximum is 100

2. `list_recycle_pages`: List up to 256 independently paginated recycle-bin pages in one bounded read-only batch
   - Parameters: `pages`, each containing `offset` and `limit`; exact logical duplicates are rejected before network access
   - Per-page limit defaults to 40; values above 100 are rejected, and the aggregate requested page budget may not exceed 5000 entries
   - Returns per-page `returned` and conservative `next_offset` metadata while preserving input order and continuing after item-level failures

3. `revertRecycleBin`: Revert selected items from the recycle bin. Requires `--allow-destructive-tools`.
   - Parameters:
     - `item_ids` (array of strings): IDs of items to revert; blanks and duplicates are rejected
     - `dry_run` (bool): Validate/preview IDs without restoring anything

4. `cleanRecycleBin`: Permanently clean selected items from the recycle bin. Requires `--allow-destructive-tools`.
   - Parameters:
     - `password` (string): Password for cleaning recycle bin
     - `item_ids` (array of strings): IDs of items to clean; blanks and duplicates are rejected
     - `dry_run` (bool): Validate/preview IDs without deleting anything; the password is never returned in the preview

### Share Tools

1. `getShareSnap`: Get shared files and directories snapshot information
   - Parameters:
     - `share_code` (string): Share code
     - `receive_code` (string): Receive code
     - `dir_id` (string): Directory ID to list, default is root directory
     - `offset` (int): Zero-based page offset, default is 0
     - `limit` (int): Page size, default is 20; values above 500 are rejected before network access

2. `get_share_snaps`: List up to 256 independently paginated share pages in one read-only batch with an aggregate 5000-entry budget
   - Parameters: `requests`, each containing `share_code`, optional/required-as-applicable `receive_code`, `dir_id`, `offset`, and `limit`
   - Per-item `limit` defaults to 20; values above 500 are rejected before network access
   - Share/receive codes are control-plane inputs only and are never returned; accidental receive-code occurrences in titles, owner names, file names, or errors are redacted
   - `next_offset` is returned for any non-empty page whose response `count` proves more entries exist, including short non-terminal pages

### Search Tools

1. `search`: Search for files and directories in the 115 cloud storage
   - Parameters:
     - `search_value` (string): Search keyword
     - `offset` (int): Offset for pagination, default is 0
     - `limit` (int): Limit number of results, default is 30; values above 500 are rejected before network access
     - `type` (int): File type filter, 0:all 1:folder 2:document 3:image 4:video 5:audio 6:archive
     - `order` (string): Sort field, e.g. file_name, user_ptime
     - `asc` (int): Ascending order, 0:descending 1:ascending

2. `search_many`: Run up to 256 independently paginated searches in one read-only batch with an aggregate 5000-result page budget
   - Parameters: `queries`, each carrying the same search fields as `search`
   - Per-query `limit` defaults to 30; values above 500 and exact logical duplicates are rejected before network access
   - Item failures are reported independently while preserving input order
   - `next_offset` is returned whenever a non-empty page's `count` proves more results exist, including short non-terminal pages

### Offline Download Tools

1. `listOfflineTasks`: List offline download tasks
   - Parameters:
     - `page` (int64): Page number for pagination, default is 1
   - The original task source URL is intentionally omitted from MCP text and structured output so query credentials/tokens are not reflected into model context

2. `list_offline_pages`: List up to 128 offline-task pages in one bounded read-only batch without reflecting source URLs
   - Parameters: `pages` (array of page numbers); page `0` normalizes to page `1`, and duplicate logical pages are rejected before network access
   - The aggregate returned-task budget is 5000; once exhausted, later pages fail closed without additional requests
   - Uses the same credential-free task shape as `listOfflineTasks`, preserving per-page order and item-level errors
   - `next_page` is returned only when the server supplies reliable `page/page_count` metadata proving another page exists; legacy responses that omit pagination metadata are not guessed

3. `addOfflineTaskURIs`: Add offline tasks by download URIs. Requires `--allow-destructive-tools`; accepted schemes are `http`, `https`, `magnet`, and `ed2k`.
   - Parameters:
     - `uris` (array of strings): Download URIs; blank, duplicate, malformed, or unsupported values are rejected before network access, and validation errors do not echo the raw URI or embedded credentials
     - `save_dir_id` (string): Directory ID to save downloaded files; non-root IDs must resolve to an existing directory
     - `dry_run` (bool): Validate all URIs and the save directory without submitting tasks; source URIs are not returned in the plan

4. `deleteOfflineTasks`: Delete offline tasks. Requires `--allow-destructive-tools`.
   - Parameters:
     - `hashes` (array of strings): Task hashes to delete; blanks and duplicates are rejected
     - `delete_files` (bool): Whether to delete associated files, default is false
     - `dry_run` (bool): Validate the request without deleting tasks or files

5. `clearOfflineTasks`: Clear offline task records. Requires `--allow-destructive-tools`.
   - Parameters:
     - `scope` (string): `completed` (default), `failed`, `active`, or `all`
     - `clear_flag` (int64): Legacy equivalent: 0 completed, 1 all, 2 failed, 3 active
     - `dry_run` (bool): Resolve the effective scope/flag without clearing tasks

## Example Request/Response

### Basic Directory Listing Request

```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "method": "tools/call",
  "params": {
    "name": "listDirectory",
    "arguments": {
      "dir_id": "0"
    }
  }
}
```

### Basic Directory Listing Response

`listDirectory` retains a compatibility JSON array in `TextContent`; new clients should prefer the stable typed `structuredContent.entries`. Thumbnail URLs are intentionally omitted from both channels.

```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "result": {
    "content": [
      {
        "type": "text",
        "text": "[{\"IsDirectory\":true,\"FileID\":\"12345\",\"ParentID\":\"0\",\"Name\":\"Documents\",\"Size\":0,\"PickCode\":\"\",\"Sha1\":\"\",\"Star\":false,\"Labels\":[],\"CreateTime\":\"0001-01-01T00:00:00Z\",\"UpdateTime\":\"0001-01-01T00:00:00Z\",\"ThumbURL\":\"\"}]"
      }
    ],
    "structuredContent": {
      "entries": [
        {
          "file_id": "12345",
          "parent_id": "0",
          "name": "Documents",
          "size": 0,
          "is_directory": true,
          "star": false
        }
      ]
    }
  }
}
```

### Search Request

Search for documents containing the word "report":

```json
{
  "jsonrpc": "2.0",
  "id": "2",
  "method": "tools/call",
  "params": {
    "name": "search",
    "arguments": {
      "search_value": "report",
      "limit": 10,
      "type": 2
    }
  }
}
```

### Search Response

```json
{
  "jsonrpc": "2.0",
  "id": "2",
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"count\":5,\"files\":[{\"file_id\":\"12345\",\"name\":\"report.pdf\",\"size\":1024,\"is_directory\":false},{\"file_id\":\"12346\",\"name\":\"annual-report.docx\",\"size\":2048,\"is_directory\":false}],\"offset\":0,\"page_size\":10}"
      }
    ]
  }
}
```

## Notes

1. Valid cookies must be provided for authentication
2. The file list in the response is returned as a text content in JSON string format
3. The Type field indicates the file type: typically 1 for directories and 0 for files
4. All tools follow the standard MCP tool calling conventions
