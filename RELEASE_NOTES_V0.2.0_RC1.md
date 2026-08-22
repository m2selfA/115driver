# v0.2.0-rc.1 Release Notes (Draft)

> Status: pre-release draft for the planned `v0.2.0-rc.1` candidate. This file does not indicate that the tag or GitHub release exists.

`v0.2.0-rc.1` is the planned first release candidate for the feature line after `v0.1.4`. The scope is a minor-version release rather than a patch: it adds broad CLI batch workflows, safe directory sync and durable recovery, a substantially larger MCP surface, Session Store v2, and a hardened release supply chain while preserving the established single-item CLI and MCP names.

## Highlights

### CLI batch and transfer workflows

- Path-oriented commands now support multi-target operation and line-delimited `--from-file` input where applicable.
- Upload, download, and share download support explicit batch concurrency while preserving the global workers-per-interface budget and fail-fast/continue-on-error semantics.
- Transfer configuration and progress reporting are shared more consistently across upload/download paths.
- The original v0.1.4 CLI command names and flag names remain available; new commands and flags are additive.

### Safe sync, sessions, and recovery

- `115driver sync` builds deterministic local/remote plans before mutation, supports `--check`, reviewed `plan_id` execution, explicit destructive approval, bounded mirror deletion, and dependency-aware execution.
- Fresh write executions use durable sync journals by default. Interrupted operations can be inspected, verified, reconciled, recovered, and resumed without blindly replaying completed work.
- Session Store v2 centralizes resumable transfer state, locking, trash, and retention policy. Valid legacy transfer session files are imported into the managed store through a validate-copy-validate flow rather than being discarded.
- Sync-journal schema v2 remains able to read v1 journals. The CLI exposes an audited v1-to-v2 migration path with source backups and crash-recoverable bulk migration markers.
- Reviewed-plan alias diagnosis and repair are available as bounded, content-addressed single-item and batch workflows with stale-token invalidation and deterministic lock ordering.

### Safer share, recycle, and offline operations

- Share browsing/download is read-only with respect to 115 cloud state and supports bounded multi-file local downloads.
- Signed share download URLs can carry required Cookie/Referer/User-Agent context internally without exposing those headers in JSON/MCP results.
- Recycle restore and offline task deletion/clear operations have explicit preview/force policies and bounded batch handling.
- Driver response validation now fails closed on malformed success payloads while retaining compatibility-sensitive public response layouts where practical.

### MCP expansion and safer authority boundaries

- All 11 v0.1.4 MCP tool names are retained, with 38 additional tools for batching, bounded tree inspection, transfer/sync planning, revalidation, execution, journal inspection/recovery, and maintenance.
- Typed `StructuredContent` results and stable machine schemas reduce dependence on parsing JSON from text content.
- Local filesystem tools require a validated `--local-root`; mutation tools require `--allow-destructive-tools`.
- `get_download_info` still exists but is now intentionally hidden unless `--allow-sensitive-tools` is set because the tool returns a short-lived signed URL into MCP content.
- Share download tools never return receive codes, signed CDN URLs, cookies, or request headers.

### Release and supply-chain certification

- Release CI is pinned to Ubuntu 24.04, Go 1.23.4, GoReleaser 2.17.1, and Syft 1.50.0 with release-critical actions pinned by commit SHA.
- Every candidate contains six platform archives, six SPDX 2.3 SBOMs, and one 12-entry SHA-256 `checksums.txt`.
- Archive verification includes host unpack/install-prefix version/help smoke.
- Tagged publication is rerun-safe across absent/draft/published states, attests provenance and SBOMs, round-trips draft bytes through GitHub before publication, and treats a published rerun as read-only verification.

## Compatibility and upgrade notes

### CLI

Machine inventory against `v0.1.4` found all 21 historical Cobra `Use` names and all 22 historical flag names still present. Existing single-target upload/download and other established invocations remain valid; batch forms are additive. Scripts should prefer `--json` and documented exit codes instead of parsing human-oriented output text.

### MCP

All 11 historical MCP tool names and all 9 historical MCP server flags remain present. Two server flags are additive: `--allow-sensitive-tools` and `--version`.

There are intentional capability-visibility changes:

- `get_download_info` requires `--allow-sensitive-tools`.
- local download/share-download tools are registered only when `--local-root` is configured.
- upload and other cloud mutations require `--allow-destructive-tools`; local upload additionally requires `--local-root`.

The legacy source entrypoint `go build ./mcp` remains buildable, but release archives and the recommended install path use the explicit `115driver-mcp-server` command.

### Go library

The R16 exported-declaration scan of `pkg/driver` found zero removals relative to `v0.1.4`. R16 restored the v0.1.4 public layouts of `QRCodeStatusResp`, `FileStatResponse`, `OfflineTaskResponse`, and `SharedDownloadInfo` after newer robustness/transfer logic had temporarily expanded or pointerized those structs.

For robust share CDN downloads, use the additive `SharedDownloadRequest` and `DownloadByShareCodeRequest*` APIs; legacy `DownloadByShareCode*` methods still return the original `SharedDownloadInfo` type.

One deliberate source-compatibility exception remains: `ListOptions` now carries private `record_open_time` state so read-only callers can force `record_open_time=0`. Keyed literals such as `driver.ListOptions{ApiURLs: ...}` and the supported option helpers remain compatible, but an external unkeyed literal such as `driver.ListOptions{[]string{...}}` no longer compiles. Replace unkeyed literals with keyed literals or `DefaultListOptions`/`WithApiURLs` helpers.

### Persisted sessions and journals

No manual migration of valid legacy transfer session files is required. Managed transfer resolution can import an existing legacy payload only after validation; the copied managed payload is validated again before the legacy file is removed best-effort.

Sync-journal storage keeps layout version `v1`, current schema version `2`, and minimum readable schema version `1`. v1 journals remain readable for inspection and have an explicit migration edge to v2. Before resuming or mutating legacy journal state, inspect `115driver sync journal schema` / `doctor` and migrate through the supported command rather than editing files manually.

## Candidate verification before publication

Before creating or pushing the real tag:

1. Review [`V0.2.0_RC1_INTEGRATION_AUDIT.md`](V0.2.0_RC1_INTEGRATION_AUDIT.md).
2. Run the manual read-only `release-dry-run` workflow for `v0.2.0-rc.1` on the exact reviewed commit.
3. Require lineage preflight to report the expected latest published release (`v0.1.4` at the time of this draft) and candidate state.
4. Review the generated Git-history release notes together with this curated draft.
5. Require the Ubuntu `release-ready-race` authority and the final-version GoReleaser/Syft artifact verification to pass.

Live destructive 115 integration is intentionally not part of the default release gate and requires a separate explicit credentialed opt-in.
