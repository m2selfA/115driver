# 115driver

A comprehensive Go library, CLI tool, and MCP server for [115 cloud storage](https://115.com). It provides a full-featured driver for 115.com's API, supporting login, file operations, upload/download, offline downloads, and more.

[![Go Report Card](https://goreportcard.com/badge/github.com/SheltonZhu/115driver)](https://goreportcard.com/report/github.com/SheltonZhu/115driver)
[![Release](https://img.shields.io/github/release/SheltonZhu/115driver)](https://github.com/SheltonZhu/115driver/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/SheltonZhu/115driver.svg)](https://pkg.go.dev/github.com/SheltonZhu/115driver)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/:License-MIT-orange.svg)](https://raw.githubusercontent.com/SheltonZhu/115driver/main/LICENSE)

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [CLI](#cli)
- [MCP Server](#mcp-server)
- [API Reference](#api-reference)
- [Troubleshooting](#troubleshooting)
- [Project Structure](#project-structure)
- [Contributing](#contributing)
- [License](#license)

## Features

**Authentication** — Cookie-based login, QR code login, and user identity verification.

**File Operations** — List, rename, move, copy, delete, download, upload (with rapid upload via SHA1 deduplication and multipart upload via Aliyun OSS), search with filters, and get file info/statistics.

**Offline Downloads** — Add HTTP, ED2K, and magnet link download tasks; list, delete, and clear tasks.

**Share** — Inspect share links and download files via share code.

**Recycle Bin** — List, restore, and permanently delete items.

**CLI** — Full-featured command-line interface with colored table output, JSON mode for scripts, shell completions, and multiple profile support.

**Safe Directory Sync** — Deterministic local/remote tree planning, read-only drift checks, destructive guardrails, concurrent dependency-aware execution, and durable journals for restartable sync/recovery.

**MCP Server** — [Model Context Protocol](https://modelcontextprotocol.io/) server for AI application integration (Claude Desktop, Cursor, etc.).

## Installation

```bash
go get github.com/SheltonZhu/115driver
```

Prebuilt GitHub release archives contain both public executables plus `README.md` and `LICENSE`. On Linux/macOS, unpack the archive for your platform and place both executables on `PATH`:

```bash
tar -xzf 115driver_<version>_<os>_<arch>.tar.gz
install -m 0755 115driver 115driver-mcp-server ~/.local/bin/
115driver --version
115driver-mcp-server --version
```

On Windows, unpack the corresponding archive and place `115driver.exe` and `115driver-mcp-server.exe` in a directory on `PATH`. Release certification performs the equivalent unpack/install-prefix version and help smoke on the host archive before publication.

## Quick Start

### Basic Usage

```go
package main

import (
    "github.com/SheltonZhu/115driver/pkg/driver"
    "log"
)

func main() {
    // Option 1: Import credentials from cookie string
    cr, err := driver.CredentialFromCookie("your_cookie_string")
    if err != nil {
        log.Fatalf("Failed to create credential: %v", err)
    }

    // Option 2: Create credentials manually
    // cr := &driver.Credential{
    //     UID:  "your_uid",
    //     CID:  "your_cid",
    //     SEID: "your_seid",
    //     KID:  "your_kid",
    // }

    // Create client with credentials
    client := driver.Default().ImportCredential(cr)

    // Validate the cookie without logging out other devices.
    if err := client.CookieCheck(); err != nil {
        log.Fatalf("Cookie validation failed: %v", err)
    }

    log.Println("Successfully logged in!")
}
```

### Common Operations

The examples below assume you have an authenticated `client` (see Basic Usage above).

```go
// Download a file using pickcode
downloadInfo, err := client.Download("pickcode_here")
if err != nil { /* handle error */ }
fileReader, _ := downloadInfo.Get()
defer fileReader.Close()
// write fileReader to file...
```

#### Empty User-Agent (UA-free CDN downloads)

115 CDN download links returned by `DownloadWithUA` may be bound to the
User-Agent used to fetch them, so tools that fetch the links without a UA
(e.g. mediainfo, openlist) require the request to carry **no** `User-Agent`
header at all (see [issue #80](https://github.com/SheltonZhu/115driver/issues/80)):

```go
// Fetch the download-URL request with no User-Agent header on the wire
info, err := client.DownloadWithUA(pickCode, "")
// info.Header["User-Agent"] is empty, matching what was actually sent
```

This is implemented client-wide in `applyEmptyUAHandling`
(`pkg/driver/client.go`) and is intentionally ad-hoc because resty provides
no option to omit its default `go-resty/<version>` User-Agent:

1. An explicitly empty UA (empty string, whitespace, or nil header value) is
   replaced with an internal sentinel before resty's middleware runs, so
   resty does not inject its default UA.
2. The sentinel is stripped from the wire request right before sending — the
   actual bytes carry no `User-Agent` header.
3. resty deep-copies request headers into the raw HTTP request, so its own
   `resp.Request.Header` keeps the sentinel internally; it is realigned to
   the actually-sent value after each response, and `DownloadInfo.Header`
   reads the raw sent request headers.

Notes:

- Do not re-add a UA to `DownloadInfo.Header` after the call, and do not
  "clean up" the returned header manually — both would mask the wire
  behavior.
- `SetHttpClient` and `WithRestyClient` replace the underlying resty client;
  the hooks are re-installed automatically, so empty-UA handling keeps
  working for every client configuration.

```go
// Upload a file (auto-selects rapid upload or multipart via OSS)
file, _ := os.Open("/path/to/local/file.zip")
defer file.Close()
fileInfo, _ := file.Stat()
uploadID, err := client.RapidUploadOrByOSS(
    "0",            // parent directory ID ("0" for root)
    fileInfo.Name(),
    fileInfo.Size(),
    file,
)
```

```go
// List files in root directory
files, err := client.List("0")
for _, f := range files {
    log.Printf("File: %s, Size: %d, Type: %s", f.Name, f.Size, f.Type)
}
```

```go
// Search for files
results, err := client.Search(&driver.SearchOption{
    SearchValue: "document",
    Limit:       100,
})
for _, r := range results.Files {
    log.Printf("File: %s, Size: %d", r.Name, r.Size)
}
```

```go
// Add offline download task
taskIDs, err := client.AddOfflineTaskURIs(
    []string{"https://example.com/file.zip"},
    "0", // "0" for root directory
)
```

## CLI

115driver includes a CLI tool for interacting with 115 cloud storage from the command line, designed for both human use (colored table output) and AI agent consumption (`--json` flag).

### Install

```bash
go install github.com/SheltonZhu/115driver/cmd/115driver@latest
```

### Authentication

```bash
# QR code login (interactive)
115driver login

# Cookie login
115driver login --cookie "UID=xxx;CID=xxx;SEID=xxx;KID=xxx"

# Verify identity
115driver whoami

# Account and storage info
115driver info
```

Credentials are stored in `~/.115driver/config.toml` and support multiple profiles.

### Authentication Priority

1. `--cookie` flag
2. `DRIVER115_COOKIE` environment variable
3. Config file (`~/.115driver/config.toml`)

Additional env vars: `DRIVER115_CONFIG` (config path), `DRIVER115_PROFILE` (profile name), `DRIVER115_SHARE_RECEIVE_CODE` (share receive code/password; an explicit `--receive-code` takes precedence).

### Commands

```bash
# List files
115driver ls /path/to/dir
115driver ls -l /path/to/dir          # detailed view

# File info
115driver stat /path/to/file

# Account and storage info
115driver info

# Remote 115 client-version metadata (distinct from local `115driver version`).
# This public metadata query does not require a saved 115 account credential.
115driver app-version

# Create directories
115driver mkdir /new/dir
115driver mkdir -p /deep/nested/dir   # create parents

# Move / Copy / Rename / Delete
115driver mv /source/file /dest/dir
115driver cp /source/file /dest/dir
115driver rename /path/to/file new_name
115driver rm /path/to/file
115driver rm /path/to/dir --force      # required for directory deletes in --json mode

# List directory contents
115driver ls /remote/dir --limit 100 --offset 0
# Text output prints a next-offset hint when a page may have more entries.
# With --json, ls includes offset, limit, has_more, and next_offset.

# Upload & Download
115driver upload /local/file /remote/dir

# Recursive upload preserves the source directory name by default:
# /local/20251004 -> /remote/dir/20251004/...
115driver upload -r /local/20251004 /remote/dir

# A trailing source separator uploads only the directory contents:
# /local/20251004/ -> /remote/dir/...
115driver upload -r /local/20251004/ /remote/dir
# --contents is the explicit, cross-platform equivalent:
115driver upload -r --contents /local/20251004 /remote/dir

# Re-running an upload checks same-name/same-size remote files by SHA1.
# Identical files are verified and skipped instead of entering rapid-upload/OSS.
115driver upload -r /local/20251004 /remote/dir

115driver download /remote/file /local/dir
115driver download /remote/file /local/dir --timeout 6h  # default 2h, 0 disables timeout

# Search (historical CLI behavior is descending; use --asc to opt into ascending order)
115driver search keyword
115driver search keyword -t video                 # filter by type
115driver search keyword --sort file_size         # sort by size, descending
115driver search keyword --sort file_size --asc   # sort by size, ascending
115driver search keyword --limit 50 --offset 100  # paginated search

# Read-only share browsing and downloading. The receive code is required for
# requests but is never echoed in the CLI result/JSON envelope. Prefer
# DRIVER115_SHARE_RECEIVE_CODE when avoiding shell-history/process-list exposure;
# an explicit --receive-code takes precedence over the environment.
115driver share ls <share_code> --receive-code <code>
115driver share ls <share_code> --receive-code <code> --dir-id <cid> --limit 50 --offset 100
115driver share download <share_code> <file_id> ./downloads --receive-code <code>
115driver share download <share_code> <file_id> <file_id> ./downloads --receive-code <code> --jobs 2 --continue-on-error
115driver share download <share_code> <file_id> ./downloads --receive-code <code> --dry-run
# `share list` and `share snap` are aliases of `share ls`.

# Recycle-bin browsing (1 <= --limit <= 100) and reversible batch restore.
115driver recycle list
115driver recycle list --limit 100 --offset 100
115driver recycle restore <item_id> <item_id>
115driver recycle restore --dry-run <item_id> <item_id>

# Offline downloads (HTTP/ED2K/magnet)
115driver offline add <url>
115driver offline add <url> -d /save/dir
115driver offline list
115driver offline rm <hash>                              # remove task record, preserve downloaded files
115driver offline rm <hash> --delete-files --dry-run
115driver offline rm <hash> --delete-files --force       # also delete associated file(s)

# Broad clearing is fail-closed: preview first, then explicitly force it.
115driver offline clear --dry-run                        # completed tasks only
115driver offline clear --force                          # clear completed tasks
115driver offline clear --scope failed --dry-run         # review failed tasks
115driver offline clear --scope failed --force           # clear failed task records
115driver offline clear --scope active --dry-run         # queued + downloading tasks
115driver offline clear --scope active --force           # clear active task records
115driver offline clear --scope all --dry-run            # review all task records
115driver offline clear --scope all --force              # clear all task records
```

### Batch and Source-List Workflows

Most path-oriented commands accept multiple targets in one invocation, and the same batch engine can read additional targets from a line-delimited file with `--from-file FILE` (`--from-file -` reads stdin). Explicit positional sources are processed before file-provided sources.

```bash
# Multiple targets in one command.
115driver ls /remote/photos /remote/docs
115driver stat /remote/a /remote/b /remote/c
115driver du /remote/photos /remote/docs
115driver mkdir /remote/a /remote/b /remote/c
115driver cp /remote/a /remote/b /remote/archive
115driver mv /remote/a /remote/b /remote/archive
115driver rm --dry-run /remote/a /remote/b

# Large source lists.
115driver upload --from-file local-paths.txt /remote/inbox
115driver download --from-file remote-paths.txt ./downloads
115driver stat --from-file remote-paths.txt
115driver ls --from-file remote-directories.txt
115driver cp --from-file remote-paths.txt /remote/archive
115driver recycle restore --from-file recycle-ids.txt
115driver share download <share_code> --receive-code <code> --from-file share-file-ids.txt ./downloads

# stdin works too. Destructive rm from stdin additionally requires --force
# unless it is a --dry-run, because stdin is otherwise needed for confirmation.
Get-Content remote-paths.txt | 115driver stat --from-file -
Get-Content remote-paths.txt | 115driver rm --from-file - --force
```

`upload`, `download`, `share download`, `mkdir`, `ls`, `stat`, `du`, `offline add`, `offline rm`, and `sessions rm` expose `--continue-on-error` for independent item-level failure handling. `upload`, `download`, and `share download` additionally support `--jobs N`; parallel batch execution requires `--continue-on-error` and shares the configured workers-per-interface budget instead of multiplying it by `N`. `cp`, `mv`, `rm`, and `recycle restore` deliberately prepare the complete source/ID set first and then submit one remote batch mutation, so they do not expose item-level `--continue-on-error` semantics.

The CLI keeps the Share surface remote-read-only: `share ls` inspects metadata and `share download` only writes local files while reusing the normal multi-interface transfer engine. Recycle supports listing plus the reversible `recycle restore` mutation; `--dry-run` validates requested IDs without restoring them. Permanent recycle-bin deletion is intentionally not exposed as an ordinary CLI command until an equivalent explicit confirmation/force contract is defined.

`offline rm` preserves downloaded files by default. `--delete-files` opts into deleting associated file data as well and is therefore fail-closed: actual execution requires `--force`, while `--dry-run` is allowed without force and reports visible `file_id` / `delete_file_id` metadata before any mutation. Multi-hash `--continue-on-error` preserves the same delete-files policy for every item.

`offline clear` deliberately exposes names instead of the driver's numeric clear flag: `--scope completed` (default), `failed`, `active`, and `all` map to 115 task-clear modes 0, 2, 3, and 1 respectively. `active` previews queued/not-yet-started plus currently downloading tasks. `--dry-run` lists the tasks currently matching the selected scope; every real clear requires `--force`. The broader 115 clear modes that also delete downloaded source files are intentionally not exposed here. Because the remote task set can change between preview and execution, the success result reports the number observed before the clear request rather than claiming an exact deletion count.

### Exit Codes and JSON Errors

The CLI uses stable process exit codes: `0` success, `1` general/runtime failure, `2` authentication failure, `3` not found, `4` network-classified failure, and `5` command-line argument or flag error. Arity errors, unknown commands, unknown flags, and command-specific validation such as an invalid `--limit` all use exit code `5` before authentication is attempted. With `--json`, failures use the same value in the top-level `code` field, so scripts can rely on the process status and JSON envelope consistently. Batch commands return a non-zero top-level status when any item fails and retain each item's more specific error code in the batch result.

### Safe Directory Sync and Recovery

`115driver sync` compares an existing local directory and an existing remote directory, builds a deterministic plan, and only then executes it. The default policy is bidirectional (`--direction both`) with unresolved divergent/type-mismatched entries treated as conflicts (`--conflict error`); it never guesses overwrite direction from timestamps.

A safe review-and-execute workflow is:

```bash
# 1. Build the plan without changing either tree.
115driver sync --dry-run ./photos /backup/photos

# 2. Re-run only if the freshly rebuilt plan still has the reviewed plan_id.
115driver sync --expect-plan <plan_id> --jobs 4 ./photos /backup/photos

# CI/read-only drift check: exit 0 only when the trees are already converged.
115driver sync --check ./photos /backup/photos
```

`plan_id` is deterministic for the captured roots, policy, actions, object identities, sizes/checksums, and modification times. Before an execution writes anything, 115driver performs a whole-tree read-only preflight; every action then repeats its stale-plan checks immediately before mutation. `--expect-plan` is therefore useful when plan review and execution are separate steps.

`--jobs N` runs dependency-ready actions concurrently while preserving directory/destructive ordering. By default, a failed execution wave prevents later waves from starting. `--continue-on-error` instead allows independent branches to continue while dependency-blocked descendants are reported as blocked; `--max-errors N` can stop that mode from launching another wave after the failure budget is reached.

For explicit conflict resolution, choose one side and acknowledge that replacement is destructive:

```bash
# Divergent/type-mismatched entries become explicit replace-remote actions
# when the selected policy/direction permits them.
115driver sync --direction upload --conflict prefer-local \
  --allow-destructive --expect-plan <plan_id> ./photos /backup/photos
```

Mirror deletion is deliberately narrower than normal sync. `--delete` requires `--direction upload` or `--direction download`; upload mode deletes remote-only targets, while download mode deletes local-only targets. Execution also requires `--allow-destructive`. Optional budgets can cap the reviewed blast radius:

```bash
115driver sync --direction upload --delete --dry-run ./photos /backup/photos

115driver sync --direction upload --delete --allow-destructive \
  --expect-plan <plan_id> \
  --max-delete-roots 10 --max-delete-items 1000 --max-delete-bytes 20GiB \
  ./photos /backup/photos
```

Directory mirror deletion is collapsed to one destructive root while covered descendants still count toward item/byte auditing. Replacements and mirror deletes are not atomic: the planned loser is removed before its winner is created, and remote deletes use the 115 recycle bin. Use the dry-run plan, `--expect-plan`, destructive approval, and budgets together for important trees.

#### Durable sync journals

Fresh write executions create a managed sync journal by default. The journal records the reviewed plan, action attempts/phases, run statistics, and verified postconditions. It is profile/account scoped and lives under the managed transfer session directory. `--dry-run` and `--check` remain read-only and do not create or update execution journals; `--no-journal` is an explicit opt-out for a fresh write execution.

If a process stops partway through a run, resume the same local and remote roots with the journal's plan ID (or a unique prefix):

```bash
115driver sync journal list
115driver sync journal inspect <plan_id>
115driver sync --resume <plan_id> --jobs 4 ./photos /backup/photos
```

Resume does not rebuild policy from new `--direction`, `--conflict`, or `--delete` flags; the stored policy is authoritative. It revalidates the whole tree with mixed evidence: completed actions must still satisfy their recorded postconditions, while pending actions must still satisfy the original preconditions. Already completed branches are skipped, so an independent failed branch can be retried without replaying successful work.

Interrupted destructive actions are fail-closed. When current evidence proves a safe outcome, resume can reconcile it automatically (including a full retry, winner-only continuation, or already-completed result). If the result is ambiguous, the journal becomes `recovery-required` and ordinary resume will not silently replay the mutation. Review it with:

```bash
# Authenticated, read-only inspection of current local + remote evidence.
115driver sync journal verify <plan_id>

# Only for a recovery-required journal whose evidence/preflight is now safe.
# This clears the recovery latch but executes no sync data actions.
115driver sync journal recover <plan_id>

# Then resume normally.
115driver sync --resume <plan_id> ./photos /backup/photos
```

`reconcile-required` means an interrupted destructive phase still needs live evidence classification; `recovery-required` is the stronger manual-review latch used when the evidence is ambiguous or cannot be safely inferred.

#### Journal administration and schema migration

| Command | Purpose |
| --- | --- |
| `sync journal schema` | Offline compatibility/capability report: journal versions, migration edges, source backups, batch-marker versions, and machine-result schemas. |
| `sync journal list [--state ...]` | Offline journal inventory with effective status, age, run statistics, and action/state/phase counters. |
| `sync journal inspect <plan_id>` | Offline full journal inspection. |
| `sync journal doctor` | Read-only offline integrity/schema/backup diagnosis, including interrupted bulk-migration markers. |
| `sync journal aliases diagnose` | Offline read-only reviewed-plan alias lifecycle diagnosis; only strictly proven orphans receive content-addressed repair IDs. |
| `sync journal aliases reconcile <review_id> --expect-repair-id <id>` | Offline exact-token orphan alias removal after re-diagnosis and locked current/trash absence proof. |
| `sync journal aliases plan [--limit N]` | Offline bounded orphan-only batch preview; `repair_set_id` binds the complete current orphan set, not only the selected prefix. |
| `sync journal aliases reconcile-batch --limit N --expect-repair-set-id <id>` | Offline exact-set batch repair using the same all-raw-locks → all-alias-locks primitive as MCP; catchable failures roll back, while abrupt process death is crash-convergent rather than power-loss atomic. |
| `sync journal verify <plan_id>` | Authenticated, read-only live-state verification before resume/recovery. |
| `sync journal recover <plan_id>` | Authenticated recovery-latch clearing after safe verification; performs no sync data actions. |
| `sync journal migrate <plan_id>` | Offline atomic upgrade of one legacy readable journal; never executes sync data actions. |
| `sync journal migrate --all` | Store-wide migration after doctor preflight; creates crash-recovery evidence before rewriting candidates. |
| `sync journal migrate --recover-batch` | Offline reconciliation of an interrupted bulk migration from exact source/target hashes and verified backups. |
| `sync journal rm <plan_id>` | Move a journal to managed trash; unresolved recovery evidence requires `--force` after review. |
| `sync journal gc [--dry-run]` | Trash old completed/failed journals while preserving in-use/recovery/bulk-migration evidence. |
| `sync journal trash list` | Offline inventory of recoverable current-v2 journals in shared Session Store trash. |
| `sync journal trash restore <plan_id>` | Offline guarded restore of one uniquely matched soft-deleted journal, including its preserved reviewed-plan aliases. |

Alias batch repair deliberately separates ordinary failure atomicity from process-crash semantics. Once the reviewed set token matches, catchable alias-removal failures are **all-or-rollback while every raw-plan and alias lock remains held**. Abrupt process termination is **not power-loss atomic**: aliases already removed before process death can remain absent. The state is nevertheless fail-safe by **monotonic convergence**: a fresh bounded diagnosis sees only the still-present orphan aliases, the old `repair_set_id` no longer matches, and stale CLI/MCP execution returns zero new mutation authority (`Requested=0`, `items=[]`) without exposing the replacement token or candidate set. The operator/agent must explicitly run a fresh `sync journal aliases plan` or fresh `plan_sync_journal_alias_repair` and review the new token before continuing.

Account scope is intentionally asymmetric. Profile-only CLI alias administration may repair persisted aliases belonging to multiple accounts inside that profile because each alias carries its own persisted account binding. Authenticated MCP alias diagnosis/repair is exact-account scoped and fails closed if a foreign-account alias binding is encountered in that profile namespace; it does not silently skip that alias or widen the authenticated authority. Use the offline CLI path to inspect/repair such mixed-account profile state.

Schema migration is intentionally separate from sync data mutation. Each newly executed migration step preserves a private, exact source-byte backup before rewriting the journal. Bulk migration writes a persistent marker before the first journal rewrite; after a crash, recovery accepts only exact pre-migration or exact migrated bytes. Unknown bytes fail closed rather than being overwritten. Journals written by newer unsupported versions are never downgraded.

For scripts, `115driver --json sync journal schema` advertises the current machine contracts instead of requiring callers to guess versions. Current result identifiers include `115driver.sync-journal-list-entry/v1`, `115driver.sync-journal-verification/v1`, `115driver.sync-journal-recovery/v1`, and `115driver.sync-journal-migration-batch-recovery/v1`. Error envelopes preserve structured result data where recovery diagnostics are available.

Sync journals contain plan/snapshot metadata such as local/remote roots, object identities, sizes/checksums, modification times, action state, and postconditions. They do **not** persist login cookies, authorization headers, signed download URLs, or other request credentials.

### Recursive Upload Path Semantics

Recursive upload follows source-object semantics by default: the source directory itself is copied into the destination directory, similar to `cp -r source destination` or `rsync -a source destination`.

```text
local/source/
└── child.bin
```

```bash
115driver upload -r /local/source /remote/destination
# -> /remote/destination/source/child.bin
```

To copy only the contents of the source directory, add a trailing `/` (a trailing `\` is also recognized on Windows) or pass `--contents` explicitly:

```bash
115driver upload -r /local/source/ /remote/destination
115driver upload -r --contents /local/source /remote/destination
# -> /remote/destination/child.bin
```

`--contents` is recommended in scripts when you want the intent to remain explicit across shells and platforms. The two modes use different effective remote destinations, so resumable transfer sessions cannot accidentally cross between "copy directory" and "copy contents" semantics.

Uploads are idempotent for already-matching files. When the destination contains a same-name regular file with the same size and a SHA1 value, 115driver hashes the local file and compares SHA1 before starting rapid-upload or OSS. A match is reported as verified/skipped; a mismatch continues through the normal upload path, reusing the digest that was already computed for verification. Files with a different size, or remote entries without a usable SHA1, are not treated as identical.

An active per-file resume state takes precedence over this fresh pre-check. This preserves resumable multipart recovery and the forced sequential compatibility path used after 115 upload verification errors such as code `10002`; the resumable upload core decides whether an interrupted target may safely be reused.

For recursive uploads in `--json` mode, `remote_dir` remains the destination argument supplied by the caller, `destination` reports the effective remote directory receiving the scanned tree, and `contents` reports which directory semantic was selected. The result also reports `uploaded`, `verified`, `skipped`, and `transferred_bytes`; an all-identical rerun can therefore complete with every file verified/skipped and zero files uploaded.

### JSON Output

All commands support `--json` for machine-readable output:

```bash
115driver --json ls /path/to/dir
115driver --json stat /path/to/file
115driver --json info
```

### Shell Completion

```bash
# Bash
echo 'source <(115driver completion bash)' >> ~/.bashrc

# Zsh
echo 'source <(115driver completion zsh)' >> ~/.zshrc

# Fish
115driver completion fish > ~/.config/fish/completions/115driver.fish
```

## MCP Server

115driver includes an MCP (Model Context Protocol) server for AI application integration (Claude Desktop, Cursor, etc.).

### Install

**Option 1: go install**

```bash
go install github.com/SheltonZhu/115driver/cmd/115driver-mcp-server@latest
```

**Option 2: build from source**

```bash
git clone https://github.com/SheltonZhu/115driver.git
cd 115driver
go build -o 115driver-mcp-server ./cmd/115driver-mcp-server
```

### Usage

```bash
# Print the MCP server build version without authentication.
115driver-mcp-server --version

# Start the installed MCP server.
115driver-mcp-server --cookie="UID=xxx;CID=xxx;SEID=xxx;KID=xxx"

# If you built the binary into the current directory, use ./115driver-mcp-server instead.

# Allow longer MCP HTTP transfers:
115driver-mcp-server --cookie="UID=xxx;CID=xxx;SEID=xxx;KID=xxx" --download-timeout=6h

# Optional MCP transfer size limits:
# All MCP download tools are unlimited by default; --download-max-bytes is enforced per file. upload_from_url defaults to 2 GiB.
115driver-mcp-server --cookie="UID=xxx;CID=xxx;SEID=xxx;KID=xxx" --download-max-bytes=0 --url-upload-max-bytes=10737418240

# Register tools that mutate 115 cloud state:
115driver-mcp-server --cookie="UID=xxx;CID=xxx;SEID=xxx;KID=xxx" --allow-destructive-tools

# Explicitly expose the sensitive signed-URL inspection tool:
115driver-mcp-server --cookie="UID=xxx;CID=xxx;SEID=xxx;KID=xxx" --allow-sensitive-tools
```

By default, the MCP server registers only remote cloud tools. `download_file` and
`download_share_file` are registered only when `--local-root` is configured; with
`--allow-destructive-tools`, `upload_from_local` and `upload_from_local_files` are likewise registered only when a local root exists. `download_files` accepts up to 256 files, validates the entire batch before data transfer, and under the `file` transfer strategy submits all files to one cross-interface scheduler run. `upload_from_local_files` accepts up to 256 local files, preflights every source plus every unique 115 target directory before the first upload, then runs each file through the existing P10 multi-interface uploader while sharing interface health state. All MCP upload entry points treat `dir_id=0` as root and verify every other target ID is an existing directory before data transfer. `upload_from_url` performs that check before fetching the external URL; `upload_from_urls` extends the same policy to up to 256 external sources, preflighting every URL/name and every unique 115 target before the first fetch, then reporting runtime failures per item without echoing source URLs.
`download_share_file` and `download_share_files` accept share credentials internally but never return the receive code, signed CDN URL, cookies, or request headers in MCP content. The batch form accepts up to 256 file IDs from one share and uses the same preflight-first scheduler path as ordinary multi-file downloads.
`get_download_info` is intentionally not registered by default because it returns a short-lived signed CDN URL directly into MCP content. Enable it only when needed with `--allow-sensitive-tools`.
`--local-root` is validated before authentication: it must already exist, resolve
through symlinks, and be a directory. The canonical root is then used for all local
path checks. Existing target paths and existing parent directories are resolved
before local reads or writes, so symlinks cannot point MCP file tools outside it.
With `--local-root`, the read-only `plan_sync` tool compares an existing local directory with an existing 115 directory through the same conservative sync classifier used by the CLI. It never executes the plan: it returns a content-addressed `MCPPlan v1`, an opaque hidden-snapshot ID, safe relative-path decisions, and aggregate transfer/delete estimates while omitting local absolute paths, SHA1 values, pick codes, signed URLs, cookies, and headers. Planning is fail-closed when the aggregate local+remote node budget is exceeded (default 1000, maximum 5000) or when required local checksumming would exceed `max_checksum_bytes` (default 4 GiB, maximum 64 GiB).
`revalidate_sync_plan` is the live-state companion to `plan_sync`. It requires the original planning parameters plus a reviewed `expect_plan_id`, reruns the complete bounded local/remote scan and classifier, and reports `gate_satisfied=true` only when the fresh plan ID still matches and the sync plan remains conflict-free. A changed plan returns `error_code=plan_changed` without returning the replacement plan ID, snapshot ID, or changed actions, forcing an explicit new `plan_sync` review. The tool is read-only; it does not execute sync or imply that destructive capability is enabled. `plan_sync` and revalidation errors redact configured local filesystem identities from MCP TextContent.
`inspect_sync_journal` is the read-only journal companion available with `--local-root`. Given a reviewed `sha256:<64hex>` plan ID, it exposes only schema/version/state/status, recovery/reconcile flags, aggregate run/item counts, and per-item relative path/action/state/phase/attempt metadata. Persisted absolute local paths, remote paths/object IDs, SHA1 values, postconditions, and raw journal error text are never returned. Journal account binding is resolved lazily with the read-only user-info endpoint when the tool is called, so merely starting MCP with `--local-root` adds no account request beyond the normal cookie check.
`diagnose_sync_recovery` is the read-only evidence classifier for any reconciliation-gated sync journal. It handles both interrupted destructive actions and ordinary `upload`/`download` actions that reached `mutation-done` but crashed before their verified postcondition was persisted. Destructive evidence is classified as `completed`, `retry-full`, `winner-only`, or `ambiguous`; non-destructive terminal writes are `completed`, `pending-observation` when the target outcome is not visible yet, or `ambiguous`. Stable evidence is bound into an opaque content-addressed `diagnosis_id` (`sha256:<64hex>`); hidden object identities/digests participate in that hash but are never returned directly. Local evidence hashing has a fixed 64 GiB budget per diagnosis/reconciliation pass: the reviewed journal is pre-budgeted before evidence I/O and actual file sizes consume the same runtime counter before hashing. Safe aggregate `checksum_budget_bytes` / `checksummed_bytes` are returned, never per-file digests. The tool requires only `--local-root`, never mutates the journal or either tree, and refuses diagnosis while the execution lock is in use. `resolvable=true` requires every checked item to have a non-ambiguous, non-pending decision; it does not itself authorize execution or clear recovery state.
`reconcile_sync_recovery` is the explicit recovery control-plane mutation and therefore requires both `--allow-destructive-tools` and `--local-root`. It accepts the reviewed `plan_id` plus the content-addressed `expect_diagnosis_id` from `diagnose_sync_recovery`, acquires the shared journal lock, re-collects all reconciliation evidence, and updates journal state only when that exact diagnosis still matches. For a verified ordinary `mutation-done` upload/download it only records the observed postcondition as succeeded; it never replays the transfer. `pending-observation` returns `postcondition_pending` with no journal change, so the caller must diagnose again later. Changed evidence returns `diagnosis_changed` **without returning the newly computed token**, forcing a fresh review; ambiguous evidence remains blocked. Reconciliation never deletes, uploads, downloads, or rewrites user content. `resume_candidate=true` only means the journal control state is eligible for the full residual preflight that `execute_sync_plan` must still pass before any content mutation. The intended recovery chain is `inspect_sync_journal` → `diagnose_sync_recovery` → `reconcile_sync_recovery` → `execute_sync_plan`.
`list_sync_executions` is the bounded read-only discovery companion for the same store. It returns up to 50 executions by default (maximum 128) after scanning at most 4096 journal files, sorted by most recent update, with an optional effective-status filter (`active`, `failed`, `completed`, `reconcile-required`, or `recovery-required`). Each entry uses the original reviewed MCP `plan_sync` plan ID reconstructed from the stored raw sync plan and exposes only safe state/count/timestamp/run/lock metadata; raw internal journal IDs, journal paths, profile/account scope, local/remote roots, remote object IDs, postconditions, raw errors, and hashes are omitted. Readable legacy journals are not decoded by MCP and contribute only to the aggregate `migration_required` count.
`diagnose_sync_journal_aliases` is the bounded read-only alias-lifecycle audit available with `--local-root`. CLI doctor and MCP now share the same `internal/syncjournal` current/trash/account lifecycle classifier; MCP adds its content-addressed reviewed-plan verifier on top. The tool scans at most 4096 profile/account-bound private reviewed-plan aliases and classifies them as `live`, `orphan`, `soft-deleted-shadow`, `identity-mismatch`, or `invalid-target`. A live target is accepted only when the target journal reconstructs to the same content-addressed MCP `plan_sync` ID as the alias. Missing current targets are cross-checked against one bounded Session Store trash pass before they can be called orphan. Only trash entries whose raw plan ID could satisfy a missing alias are decoded; malformed/incomplete **matching** trash evidence fails the diagnosis closed so a soft-deleted journal cannot be misreported as absent, while unrelated damaged trash still counts toward the scan bound but does not block proof for another alias. Only a strictly proven `orphan` item receives an opaque content-addressed `repair_id`; that token binds the hidden raw plan ID, exact profile/account binding, alias creation/update timestamps, and orphan status without returning any of those fields. Other statuses never receive a repair token. Output otherwise contains reviewed MCP plan IDs and safe status/error codes only; raw journal IDs, paths, account/profile metadata, roots, hashes, postconditions, and sidecar contents are never returned. The diagnosis itself never repairs or deletes aliases. Local `sync journal doctor` performs the analogous offline shared audit and reports orphan or soft-deleted alias states as issues without rewriting them. For targeted offline maintenance, `sync journal aliases diagnose` exposes the same shared lifecycle classification and orphan-only repair token, while `sync journal aliases reconcile <review_id> --expect-repair-id <id>` re-diagnoses and applies the same exact-snapshot plus locked current/trash absence gates before removing an orphan alias; neither CLI subcommand authenticates or touches either sync tree. CLI and MCP both derive that token through the same `internal/syncjournal.ReviewAliasRepairID`, so one exact orphan snapshot has one repair-token meaning across both frontends; CLI remains profile-scoped/offline, while MCP additionally applies its content-addressed live-plan identity verifier.
`reconcile_sync_journal_alias` is the explicitly reviewed alias-control mutation and therefore requires both `--allow-destructive-tools` and `--local-root`. It accepts the orphan reviewed `plan_id` plus the exact `expect_repair_id` from `diagnose_sync_journal_aliases`, reruns the full bounded lifecycle diagnosis, and refuses every status except `orphan`. A changed alias snapshot returns `repair_changed` without returning a replacement token, forcing a new diagnosis/review. Before removal it follows the canonical raw-journal → alias lock order, requires the entire persisted alias record/timestamps to still equal the reviewed snapshot, and re-proves that neither the canonical current journal nor a matching Session Store trash journal exists. A `soft-deleted-shadow` returns `journal_trashed` and must use the restore workflow; `identity-mismatch` returns `journal_alias_conflict` and is never auto-repaired. Successful reconciliation removes only the private reviewed-plan alias metadata; it never uploads, downloads, deletes, or rewrites local/115 user content, and its output never exposes the hidden raw journal identity, profile/account binding, paths, hashes, or trash evidence.
`plan_sync_journal_alias_repair` is the bounded read-only batch companion for orphan-alias maintenance. It reruns the same shared lifecycle diagnosis, selects only proven `orphan` aliases, and returns at most `limit` candidates (`50` by default, maximum `128`) plus a content-addressed `repair_set_id`. The set token deliberately binds the **complete currently diagnosed orphan set**, not just the selected prefix, as well as the requested limit; therefore a newly created, removed, or rewritten orphan outside the selected first N invalidates the reviewed batch before any mutation. Each candidate still carries its individual opaque `repair_id`; live, soft-deleted, identity-mismatched, and invalid aliases never become candidates.
`execute_sync_journal_alias_repair` is the destructive-capability half of that batch protocol and requires both `--allow-destructive-tools` and `--local-root`. It requires the same `limit` plus `expect_repair_set_id`, rebuilds the complete bounded diagnosis and performs **zero alias removals** when the set token changed; stale execution reports `Requested=0` with `items=[]`, and the replacement token/candidate set is not returned, so callers must run a fresh `plan_sync_journal_alias_repair` and review it again. After the batch gate matches, the shared exact-batch primitive sorts and locks **all distinct raw plan IDs first, then all reviewed alias IDs**, re-reads every complete persisted alias snapshot, proves every canonical current target absent, and performs one bounded matching-trash proof for the whole selected set before removing the first alias. A stale snapshot, late current/trash appearance, or lock contention therefore leaves the entire selected set `unchanged`. If alias-file removal fails after mutation begins, already removed aliases are rewritten from their exact reviewed snapshots while all locks are still held; a successful rollback again reports the whole set unchanged. Abrupt process termination is intentionally **not power-loss atomic**: aliases removed before process death can remain removed, but a fresh diagnosis/plan monotonically converges on the remaining orphan set and invalidates the old token before any new removal. Only failure of an in-process rollback itself can produce `unknown>0`, `partial=true`, and `recovery_required=true`, which requires a fresh alias lifecycle diagnosis before any further repair. The batch never includes or automatically repairs `soft-deleted-shadow`, `identity-mismatch`, or `invalid-target` states and never touches local/115 user content. Offline CLI administration exposes the same shared set-token and exact-batch protocol as `sync journal aliases plan [--limit N]` → `sync journal aliases reconcile-batch --limit N --expect-repair-set-id <id>`; Profile-only CLI execution may span persisted accounts within that profile, while authenticated MCP execution remains strictly account-bound and fails closed rather than skipping a foreign-account alias binding.
`plan_sync_journal_cleanup` is the read-only retention-GC preview for the same store. It uses the shared CLI/MCP GC eligibility rule and only selects sufficiently old `completed` or `failed` current-v2 journals; in-use, active, recovery-required, reconcile-required, and too-recent journals are never candidates. `older_than_hours=0` uses `[transfer.sessions].retention` and then the historical 30-day fallback; `limit` defaults to 50 and is capped at 128. A CLI bulk-migration marker disables MCP cleanup planning entirely. Output contains reviewed MCP plan IDs and safe age/run metadata only, plus a content-addressed `cleanup_id` for the exact selected set; this preview does not move or delete any journal.
`execute_sync_journal_cleanup` is the reviewed mutating half and requires both `--allow-destructive-tools` and `--local-root`. It reruns the same cleanup preview under the shared sync-journal `gc.lock` and CLI bulk-migration lock, requires an exact `expect_cleanup_id`, and then revalidates every candidate again under its journal lock before moving it into the common Session Store `trash/` area. Before the move, every current reviewed-plan alias for the raw sync journal plus the reviewed ID that authorized this cleanup is collected, deterministically locked, and persisted in a private `review-aliases.json` trash sidecar; all live alias bindings are then removed. A sidecar/alias/stamp failure attempts to roll the journal and the complete alias set back together. A changed cleanup set fails before the first move; a later per-item race stops the remaining batch and returns explicit partial/failed/skipped status. The tool never deletes user files or 115 objects and never exposes raw journal/trash paths, internal plan IDs, the hidden alias set, roots, object IDs, hashes, postconditions, or raw errors.
Cleanup is a true soft-delete window rather than an immediate purge: CLI current-v2 `sync journal rm/gc` and MCP cleanup share the same alias-aware trash primitive, stamp the destination trash directory with the actual move time, and shared Session Store GC applies `trash_retention` from that timestamp. Readable legacy CLI journals retain their historical raw soft-delete fallback and are never migrated merely to remove them. `list_sync_journal_trash` is the bounded read-only recovery view for current-v2 `sync-journal-*` trash belonging to the authenticated profile/account. It exposes reviewed MCP plan IDs, safe state/run timestamps, an opaque content-addressed `restore_id`, plus `trash_age_ms`, `trash_retention_ms`, `purge_eligible_at`, and `purge_eligible`. The purge timestamp is only the point at which opportunistic Session Store GC becomes allowed to remove the entry; it is not a promised deletion time. The restore token also binds the hidden complete reviewed-alias sidecar, so changing that sidecar invalidates an old token even if directory mtime is restored. Raw trash names/paths, internal plan IDs, alias contents, roots, object IDs, hashes, postconditions, and raw errors stay hidden. `restore_sync_journal` requires both `--allow-destructive-tools` and `--local-root`, acquires the same GC/bulk-migration guard, requires the exact reviewed `plan_id` + `expect_restore_id`, rechecks the journal/update/trash-time/alias-set snapshot under the raw-plan lock, requires the canonical current location to still be absent, restores the journal, and recreates/verifies the complete alias set. Current/alias collisions or changed trash evidence fail closed and leave the trash entry intact. If cleanup crashes after moving a current journal into trash but before sidecar/live-alias cleanup completes, stale-alias repair first proves that no matching trash exists; a matching soft-deleted journal returns `journal_trashed` and cannot fall through to fresh execution. The CLI also exposes `sync journal trash list` and `sync journal trash restore <plan_id>` for local raw-ID administration; raw CLI restore uses the same guarded shared restore and recreates every alias stored in the sidecar. Restore never changes user files or 115 objects, and it preserves any recovery/reconcile-required status on the journal.
When `[transfer.sessions].auto_gc` is enabled, persistent MCP sync execution and journal-cleanup execution also invoke the existing SessionStore opportunistic GC best-effort using the configured `gc_interval`, `retention`, and `trash_retention`. This maintains ordinary transfer sessions and the shared `trash/` namespace, including old `sync-journal-*` trash produced by reviewed cleanup. It deliberately does **not** auto-select current sync journals: current journals remain removable only through the reviewed `plan_sync_journal_cleanup` → `execute_sync_journal_cleanup` `cleanup_id` flow, preserving alias/journal locking and anti-replay guarantees.
`execute_sync_plan` closes the reviewed sync loop only when both `--allow-destructive-tools` and `--local-root` are enabled. For a fresh reviewed plan it performs the original two live replan gates. When the reviewed external plan ID already has a private journal alias, execution opens that exact current-v2 journal under the shared raw-plan lock and reconstructs the journal's content-addressed MCP plan envelope; the alias ID must match that reconstructed reviewed plan ID or execution fails with `journal_alias_conflict` before any resume state is changed. It then validates the stored roots/policy, the original destructive budgets, the whole expected local/remote tree, and every completed postcondition before building a residual plan; a second identical journal-state gate runs at the shared `internal/syncexec` barrier immediately before the first residual write. If an alias target current journal is missing, repair is allowed only after proving both current and matching Session Store trash are absent; a matching soft-deleted journal returns `journal_trashed` instead of being mistaken for a fresh plan. Completed items stay completed and are scheduled only as idempotent skips, while safe unfinished non-destructive items are retried without replaying successful work. A changed completed object or any unexpected/missing tree entry returns `journal_state_changed` before mutation. A nonterminal `mutation-done` without a verifiable postcondition returns `journal_reconcile_required` rather than being reset. Destructive directory delete/replace roots use the same shared `internal/syncguard` subtree validator as CLI sync: the root identity/type is rechecked first, every planned descendant must still exist with the expected identity/type/size/SHA1-or-mtime metadata, and any unplanned deep descendant blocks `Delete`/`RemoveAll` before mutation. Replacement additionally validates the winning side before removing the old subtree. Optional execution-only `max_delete_roots`, `max_delete_items`, and `max_delete_bytes` bound all destructive removals, including mirror-delete roots and replacement losers; resume validates those limits against the original reviewed plan before running its smaller residual. `jobs` defaults to 1 and is capped at 16; independent metadata/directory/file-delete actions may run in parallel, while upload/download data paths are additionally serialized to one transfer slot. `continue_on_error` and `max_errors` use the same dependency-branch blocking semantics as CLI sync. Execution is not transactional: already-completed actions are not rolled back after a later failure. If a destructive mutation can no longer be proven complete or untouched, the typed result sets `recovery_required=true`; use `inspect_sync_journal` and `diagnose_sync_recovery` to observe stable evidence, apply only an exact reviewed `diagnosis_id` through `reconcile_sync_recovery`, and then return to `execute_sync_plan` for the full residual gates rather than blindly replaying the old plan. MCP sync execution uses the same profile/account-scoped current-v2 journal layout and `sync-locks` protocol as CLI sync, persists per-item attempts/phases and verified postconditions, refuses concurrent same-plan execution with `journal_in_use`, and delegates legacy readable journals to CLI migration with `journal_migration_required`. Typed output exposes only safe journal summary fields (`journal_persisted`, `journal_resumed`, version/state/status/counts), never the raw internal journal plan ID, journal path, local absolute paths, remote IDs, or content hashes.
`plan_transfer` is the companion read-only transfer planner under `--local-root`. It accepts a mixed batch of local uploads and ordinary 115 downloads (256 items maximum in aggregate), reuses the same full-batch preflight as `upload_from_local_files` and `download_files`, and returns a content-addressed `MCPPlan v1` without moving data. Local upload sources and already-existing local download targets are content-snapshotted: canonical identity, size, and SHA-256 content are bound into opaque preconditions without returning paths or digests. The aggregate bytes for all such local content snapshots are budgeted before the first hash (default `max_checksum_bytes` 4 GiB, maximum 64 GiB), so a same-size/same-mtime rewrite of either an upload source or a destructive download target changes the plan ID. Download pick codes are likewise hidden inside opaque source snapshots. Signed URLs, headers, cookies, and user agents are omitted. Upload operations are additive, while a download whose validated local target already exists is classified destructive in the plan.
`revalidate_transfer_plan` is the read-only live-state companion to `plan_transfer`. It repeats the original mixed upload/download preflight and content snapshots under the same checksum budget, then reports `gate_satisfied=true` only when the fresh plan still equals the reviewed `expect_plan_id`. A mismatch returns `error_code=plan_changed` without exposing the replacement plan ID, changed operations, local paths, pick codes, signed URLs, or content digests; callers must run `plan_transfer` again to review the new state.
`upload_from_local`, `download_file`, `upload_from_local_files`, and `download_files` can optionally require a reviewed single-direction `plan_transfer` result through `expect_plan_id`; after their normal preflight they rebuild the corresponding transfer plan from the same prepared source/target state and fail before the first upload/download data-path call when the plan ID differs. The gated single and batch forms accept `max_checksum_bytes` so planning and execution can use the same local freshness-read budget; the shared batch/planner download item shape remains gate-free, so `download_file` exposes its gate only at the single-tool top level. For a mixed upload+download plan, `execute_transfer_plan` is available only with both `--allow-destructive-tools` and `--local-root`: it requires `expect_plan_id`, preflights both phases before mutation, rebuilds and matches the whole mixed plan, then executes uploads before downloads. If any upload item fails, the download phase is explicitly skipped. These gates detect stale/tampered planning state but are not filesystem locks or all-or-nothing transactions.
The default read-only `validate_plan` tool statically verifies any `MCPPlan v1`: content-addressed `plan_id`, canonical normalization, dependency DAG, safety class, byte estimates, and precondition ownership. Validation performs no local or 115 state I/O, so `valid=true` proves structural/integrity validity only; it does not prove that the planner's local or remote preconditions are still fresh. Invalid plans return sanitized error codes/messages without reflecting operation refs or opaque expected values.
Tools that create, upload, move, rename, delete, clean recycle bin items, or
add/remove offline tasks require `--allow-destructive-tools`. File mutations (`mkdir`, `delete`, `rename`, `move`, `copy`) support `dry_run` after read-only preflight; `mkdir_many` and `rename_many` extend the same contract to up to 256 items and optionally continue after runtime item failures with `continue_on_error`. `delete`, `move`, and `copy` still submit their validated multi-ID source set through one 115 mutation request. Recycle and offline mutation tools also support `dry_run`; recycle-clean previews never echo the password, and offline-add previews report only safe counts/target metadata rather than the source URIs. All four upload entry points support `dry_run` as well: local previews validate the source file and 115 destination but never invoke P10, while URL previews stop before DNS/HTTP fetch and omit the source URL from MCP content.
`listDirectory` explicitly disables 115 `record_open_time`, including both paged and unpaged reads. Mutation name preflight uses the same read-only listing mode before the first write.
The default read-only surface also includes bounded batch and traversal tools. `list_directories` accepts up to 256 independently paginated directory pages with an aggregate 5000-entry page budget; `search_many` and `get_share_snaps` apply the same 256-request/5000-entry batch discipline and return `next_offset` whenever a non-empty page is known to have more results, including valid short pages. `get_share_snaps` never returns share or receive codes. `list_recycle_pages` adds independently paginated recycle-bin batch reads with a 5000-entry aggregate request budget, while `list_offline_pages` batches up to 128 offline-task pages with a 5000-task output budget and the same source-URL redaction as `listOfflineTasks`. `resolve_paths` resolves up to 256 remote paths to stable object IDs, while `inspect_paths` resolves and enriches up to 128 paths with a safe metadata view that excludes thumbnail/signed URLs and distinguishes resolution from metadata-completeness failures. `list_tree` recursively lists up to 32 roots with a default 1000-node and maximum 5000-node aggregate budget. `compare_directories` compares two directory trees with the same 1000/5000 aggregate node bounds, starts from a fair two-sided budget, reuses unused budget, and never turns a node-truncated or failed opposite side into a false `only_left`/`only_right` claim; such unmatched entries are returned as `unverified_left`/`unverified_right`. Stable comparison metadata is file size, SHA1, and star state; remote IDs, pick codes, and timestamps are not treated as equality keys. `summarize_usage` summarizes up to 64 paths with a default 10000-node and maximum 50000-node aggregate budget. Traversal tools report explicit depth/node-limited partial results instead of presenting truncated data as complete. `get_app_versions` exposes currently advertised 115 application versions through the same read-only surface.
All registered MCP tools expose typed `outputSchema` / `structuredContent` while retaining JSON `TextContent` for compatibility with existing clients. Sensitive control-plane data is excluded from typed output as well: account output omits raw `imei_info`, share outputs omit credentials, and `listOfflineTasks` omits original task URLs.
For `clearOfflineTasks`, prefer the named `scope` values `completed`, `failed`, `active`, or `all`; legacy numeric `clear_flag` values 0–3 remain accepted. Modes outside that task-only set are rejected so MCP cannot opt into broader 115 clear modes that may delete downloaded source files.
`addOfflineTaskURIs` accepts only `http`, `https`, `magnet`, and `ed2k` URIs. Blank, duplicate, malformed, or unsupported URIs fail preflight before any network request, and URI text/credentials are not echoed in those validation errors.
`upload_from_url` and `upload_from_urls` only accept HTTP/HTTPS URLs, reject redirects to unsafe
hosts, and block loopback/private/link-local resolved addresses. If a hostname
resolves to multiple safe addresses, MCP HTTP transfers try later addresses when
an earlier address cannot be reached.

### Available Tools

| Category | Tools |
|----------|-------|
| **Account / Metadata** | `getAccountInfo`, `get_app_versions` |
| **Directory / Traversal** | `listDirectory`, `list_directories`, `resolve_paths`, `inspect_paths`, `list_tree`, `compare_directories`, `summarize_usage` |
| **File / Planning** | `stat`, `stat_many`, `validate_plan`; with `--local-root`: `inspect_sync_journal`, `diagnose_sync_journal_aliases`, `diagnose_sync_recovery`, `list_sync_executions`, `list_sync_journal_trash`, `plan_sync_journal_alias_repair`, `plan_sync_journal_cleanup`, `plan_sync`, `revalidate_sync_plan`, `plan_transfer`, `revalidate_transfer_plan`, `download_file`, `download_files`; with `--allow-sensitive-tools`: `get_download_info`; with `--allow-destructive-tools`: `mkdir`, `mkdir_many`, `delete`, `rename`, `rename_many`, `move`, `copy`, `upload_from_url`, `upload_from_urls`; with both destructive tools and `--local-root`: `upload_from_local`, `upload_from_local_files`, `execute_transfer_plan`, `execute_sync_plan`, `execute_sync_journal_alias_repair`, `execute_sync_journal_cleanup`, `restore_sync_journal`, `reconcile_sync_journal_alias`, `reconcile_sync_recovery` |
| **Search** | `search`, `search_many` |
| **Offline** | `listOfflineTasks`, `list_offline_pages`; with `--allow-destructive-tools`: `addOfflineTaskURIs`, `deleteOfflineTasks`, `clearOfflineTasks` |
| **Share** | `getShareSnap`, `get_share_snaps`; with `--local-root`: `download_share_file`, `download_share_files` |
| **Recycle** | `listRecycleBin`, `list_recycle_pages`; with `--allow-destructive-tools`: `revertRecycleBin`, `cleanRecycleBin` |

All registered MCP tools expose typed `outputSchema`/`structuredContent` while retaining JSON `TextContent` for compatibility. Default read-only outputs deliberately omit credential-bearing source URLs, share receive codes, raw device identifiers, and short-lived avatar/thumbnail CDN URLs; `get_download_info` is the explicit `--allow-sensitive-tools` exception for callers that need a signed download URL.

Path-oriented MCP reads (`resolve_paths`, `inspect_paths`, `list_tree`, `compare_directories`, `summarize_usage`, `plan_sync`, `revalidate_sync_plan`) share identical directory pages only through a request-scoped snapshot. It retains at most 50,000 file entries, reserves at most 100,000 new paged-list entries, never crosses tool calls, and rejects the next uncached page before network access when the remote-read budget is exhausted; cache hits and concurrent waiters do not consume that budget again.

### Configure with Claude Desktop

Add to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "115driver": {
      "command": "115driver-mcp-server",
      "args": ["--cookie=UID=xxx;CID=xxx;SEID=xxx;KID=xxx"]
    }
  }
}
```

## API Reference

For detailed API documentation, visit [pkg.go.dev](https://pkg.go.dev/github.com/SheltonZhu/115driver).

## Troubleshooting

### Login Issues

If you encounter login issues:
1. Make sure your cookie is valid and not expired
2. Check that UID, CID, and SEID are present; include KID when your cookie provides it (legacy cookies without KID remain supported)
3. Try logging in through the web interface first to obtain a fresh cookie

### Upload/Download Issues

If upload or download fails:
1. Verify file paths are correct
2. Check your internet connection
3. Ensure you have sufficient storage space
4. Check the returned error message for specific details

### Rate Limiting

The 115 API may have rate limits. If you encounter rate limiting errors:
1. Add delays between operations
2. Implement retry logic with exponential backoff
3. Consider using a proxy if needed

## Project Structure

```
115driver/                    # Go 1.23+
├── cmd/
│   ├── 115driver/            # Canonical CLI entry point
│   ├── 115driver-mcp-server/ # Canonical MCP go-install/release entry point
│   ├── release-notes/        # Release-note generator
│   └── release-preflight/    # Read-only release state/preflight simulator
├── cli/                      # CLI implementation
│   ├── cmd/                  # Cobra commands
│   └── internal/             # Internal packages (auth, output, resolver)
├── internal/
│   ├── mcpapp/               # Shared MCP command startup/config logic
│   └── releaseops/           # Pure release state, asset-set, and RC channel model
├── pkg/
│   ├── driver/               # Core driver (client, login, file, upload, download, search, share, offline)
│   └── crypto/               # Cryptography utilities (ECDH, AES, RSA)
└── mcp/
    ├── main.go               # Backward-compatible legacy source entry point
    └── server/tools/         # MCP tool implementations (account, dir, file, search, offline, share, recycle)
```

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=sheltonzhu/115driver&type=date&legend=top-left)](https://www.star-history.com/#sheltonzhu/115driver&type=date&legend=top-left)

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

### Testing

The default test suite is safe to run without a 115 account:

```bash
go test ./...
# or
make test
```

Before freezing sync/session changes, use the cross-platform non-race release gate `make release-ready`. It verifies downloaded module checksums, runs the complete offline Go test suite and `go vet`, executes the deterministic sync/session certification, and builds both entry points with `CGO_ENABLED=0`. The inner `test-sync-session-cert` keeps the dedicated alias-repair 10x crash/concurrency matrix, then repeats Session Store/GC, shared sync execution hard-write barriers, destructive subtree guards, journal migration recovery, CLI resume/reconciliation, MCP persistent execution/recovery, and MCP startup/session-scope contracts five times by default. Release packaging also installs both public commands into a temporary `GOBIN`, exercises their unauthenticated version/help surfaces, runs the release-note generator against a temporary tagged Git history, and cross-builds both entry points for every GoReleaser target: Linux, macOS, and Windows on amd64 and arm64.

```bash
make release-ready
# Focused sync/session certification while iterating on this subsystem.
make test-sync-session-cert
# Focused alias-only soak remains available when changing alias lifecycle code.
make test-sync-journal-alias-cert ALIAS_REPAIR_CERT_COUNT=20
# The non-alias sync/session repetitions are independently tunable.
make test-sync-session-cert SYNC_SESSION_CERT_COUNT=10
```

On a CGO/race-capable LF checkout, `release-ready-race` adds `go mod tidy -diff` plus the mandatory race-detector matrix after the complete non-race release gate. Repository `.gitattributes` pins `go.mod` and `go.sum` to LF so fresh CI/release checkouts make the tidy comparison byte-stable:

```bash
make release-ready-race
```

GitHub Actions runs `release-ready-race` on the pinned `ubuntu-24.04` artifact lanes for ordinary branch/PR certification **and again before tagged publication**. Those lanes pin Go `1.23.4`, GoReleaser `v2.17.1`, Syft `v1.50.0`, and the release-critical checkout/setup-go/GoReleaser/Syft-download/attestation actions to full Git commit SHAs, while explicitly enabling `CGO_ENABLED=1` for the mandatory tidy-diff/race result. GoReleaser emits exactly six `tar.gz` platform archives and six archive-level SPDX 2.3 JSON SBOMs; its single SHA-256 `checksums.txt` binds all twelve generated files. `make verify-release-artifacts` requires that complete 6+6 set, validates every manifest digest and core SPDX document metadata, checks archive contents and one consistent non-development version, unpacks the host archive into a temporary installation prefix, and executes the installed CLI/MCP `--version`/`--help` surfaces. PR certification builds the pinned snapshot twice and compares only the six archive checksum records byte-for-byte: the archives are intentionally reproducible through commit-time build/archive metadata, while Syft-generated SPDX documents are validated and checksummed on every run but are not incorrectly required to be byte-identical across separate cataloging runs. The tag workflow is stricter and rerun-safe. A same-tag concurrency group serializes runs, the tag must resolve to `GITHUB_SHA`, and the workflow first classifies the GitHub release as `absent`, `draft`, or `published`. An already published release takes the read-only path: no Go toolchain, test gate, Syft, GoReleaser, attestation creation, upload, or edit step runs; instead all thirteen release assets (six archives, six SBOMs, and `checksums.txt`) are downloaded, their twelve checksums are revalidated, and both SLSA provenance and SPDX SBOM attestations must verify against the exact repository, workflow, commit, and tag ref. For an absent or draft release, the pinned GoReleaser builds the final-version bytes once, the verifier requires `${GITHUB_REF_NAME#v}`, generic provenance covers all twelve checksummed files, the checksum manifest is attested separately, and each archive receives its own SPDX SBOM attestation. An absent release is staged as a draft; an existing draft is repaired by `gh release upload --clobber` only for the thirteen expected asset names. Unknown extra draft assets therefore survive repair and make the exact asset-count gate fail closed. Before publication, GitHub is used as the source for a full remote round trip: the workflow downloads the draft, requires 6 archives + 6 SBOMs + the 12-entry manifest, byte-compares `checksums.txt`, and re-runs all SHA-256 checks. Only then does `gh release edit --draft=false --verify-tag` publish, so there is no second GoReleaser build and a crash at candidate build, attestation, upload, or draft verification can converge safely on a rerun. Weekly grouped Dependabot updates are enabled for GitHub Actions so SHA-pinned actions can be reviewed and advanced deliberately. Consumers can independently verify downloaded artifacts with `gh attestation verify <artifact> -R OWNER/REPO`, where `OWNER/REPO` is the exact GitHub repository from which the release was downloaded; this matches the workflow's `$GITHUB_REPOSITORY` attestation authority. A long-lived Windows checkout created before the LF attributes were applied may still have CRLF module files; in that case `make release-ready` remains the accurate local gate, while fresh Linux CI/release checkout supplies the byte-stable tidy, race, real Syft SBOM, final-version candidate, provenance, and exact-byte promotion results. The focused alias targets remain prerequisites of the broader sync/session gates, so alias lifecycle cannot be bypassed by the release authority.

Before creating a release tag, maintainers can run the manual `release-dry-run` GitHub Actions workflow with a proposed SemVer tag such as `v0.2.0-rc.1` for the current post-`v0.1.4` feature scope (or another deliberately chosen version bump). That workflow has only `contents: read`: it reads the actual tag/release state, runs the pure `release-preflight` model for the actual state and for simulated `absent`, partial-`draft`, and `published` states against the same real GitHub release history, executes `release-ready-race`, creates the proposed tag only inside the runner, builds one final-version GoReleaser candidate with `--skip=publish`, verifies the six archives/six SBOMs plus install smoke, and renders the release notes. It cannot create, upload, edit, publish, or attest a GitHub release. For every new or repairable draft candidate, the preflight also computes `latest_published_tag` from published SemVer releases and requires the proposed tag to advance that lineage; an exact already-published tag remains a read-only rerun exception. If the latest published version is a prerelease and the next tag is stable, the stable tag must keep the same `MAJOR.MINOR.PATCH` core and reports that source as `promotion_from`, preventing cross-line RC promotion. The production tag workflow runs the same lineage preflight before the release-readiness/build path. It also derives the release channel from the SemVer tag: tags with a prerelease component (for example `-rc.1`, `-beta.2`, or `-alpha.1`) must carry GitHub `isPrerelease=true`, while stable `vMAJOR.MINOR.PATCH` tags must carry `isPrerelease=false`; create, draft repair, staged verification, published verification, and rerun verification all enforce that bit. Operational steps for the v0.2.0 release line are archived in [`RELEASE_CHECKLIST.md`](docs/release/v0.2.0/RELEASE_CHECKLIST.md).

The `v0.2.0-rc.1` pre-RC product/compatibility boundary is archived in [`V0.2.0_RC1_INTEGRATION_AUDIT.md`](docs/release/v0.2.0/V0.2.0_RC1_INTEGRATION_AUDIT.md), with the maintainer-facing candidate notes in [`RELEASE_NOTES_V0.2.0_RC1.md`](docs/release/v0.2.0/RELEASE_NOTES_V0.2.0_RC1.md). The audit records the machine surface inventory against `v0.1.4`, the retained CLI/MCP names and flags, the guarded session/journal upgrade path, the restored compatibility-sensitive `pkg/driver` layouts, and the one accepted Go source exception for external unkeyed `ListOptions` literals.


R17 freezes the corresponding commit-ready integration boundary in [`V0.2.0_RC1_COMMIT_MANIFEST.json`](docs/release/v0.2.0/V0.2.0_RC1_COMMIT_MANIFEST.json). Before the candidate tag exists, `make verify-rc-commit-boundary` evaluates the pre-release HEAD/worktree candidate; after `v0.2.0-rc.1` exists, it verifies the immutable historical `v0.1.4..v0.2.0-rc.1` path set instead, so post-release development does not rewrite or invalidate the 402-path RC1 evidence. The dependency-safe order remains `driver` (46 paths) -> `core` (96) -> `cli` (103) -> `mcp` (125) -> `release` (32), with the frozen counts, hashes, and commit subjects preserved in the archived manifest.

R18 extends the same authority to the Git index without adding a new candidate path. `make verify-rc-index-empty` requires no staged changes, while `make verify-rc-staged-layer RC_COMMIT_LAYER=<layer>` requires the index path-set to equal exactly the selected frozen layer. `go run ./cmd/release-boundary-check -print-layer <layer> -print-layer-nul` emits a NUL-delimited pathspec for `git add -A --pathspec-from-file=<file> --pathspec-file-nul`, so the eventual materialization can avoid shell quoting ambiguity. The index checker always revalidates the complete 402-path boundary first and uses `--no-renames` path semantics. It does not stage or commit anything; maintainers still review `git diff --cached --check`, `--stat`, and `--summary` before each commit.

Live `pkg/driver` integration tests are deliberately excluded unless both `RUN_115_INTEGRATION=1` and `COOKIE` are present. Tests that can mutate cloud state require an additional `RUN_115_DESTRUCTIVE_INTEGRATION=1` opt-in. This lets you run a bounded authenticated read-only smoke without also authorizing directory creation, uploads, recycle-bin changes, or offline-task mutations.

Prefer the read-only target first:

```bash
make test-integration-readonly COOKIE='UID=...;CID=...;SEID=...;KID=...'
```

The full integration target includes destructive tests and may mutate the authenticated 115 account:

```bash
make test-integration COOKIE='UID=...;CID=...;SEID=...;KID=...'
```

The read-only target runs the dedicated `TestReadOnlyIntegrationSmoke`, which checks authentication plus user/account info, root listing, recycle-bin listing, and offline-task listing without issuing cloud mutations. Its directory listing explicitly disables 115's `record_open_time` request flag so the smoke does not ask the service to record directory-open metadata. For direct `go test` invocation, use `RUN_115_INTEGRATION=1 COOKIE='...' go test -count=1 -run '^TestReadOnlyIntegrationSmoke$' ./pkg/driver`; add `RUN_115_DESTRUCTIVE_INTEGRATION=1` only when destructive integration is intentional.

Repository maintainers can run the same bounded smoke from GitHub Actions via the manual `integration-readonly` workflow after configuring the `DRIVER115_INTEGRATION_COOKIE` repository secret. That workflow forces `RUN_115_DESTRUCTIVE_INTEGRATION=0`, has read-only repository permissions, and never runs the destructive integration suite.

## Contributors

<!-- readme: contributors -start -->
<table>
<tr>
    <td align="center">
        <a href="https://github.com/SheltonZhu">
            <img src="https://avatars.githubusercontent.com/u/26734784?v=4" width="100;" alt="SheltonZhu"/>
            <br />
            <sub><b>SheltonZhu</b></sub>
        </a>
    </td>
    <td align="center">
        <a href="https://github.com/xhofe">
            <img src="https://avatars.githubusercontent.com/u/36558727?v=4" width="100;" alt="xhofe"/>
            <br />
            <sub><b>xhofe</b></sub>
        </a>
    </td>
    <td align="center">
        <a href="https://github.com/Ovear">
            <img src="https://avatars.githubusercontent.com/u/1362137?v=4" width="100;" alt="Ovear"/>
            <br />
            <sub><b>Ovear</b></sub>
        </a>
    </td>
    <td align="center">
        <a href="https://github.com/power721">
            <img src="https://avatars.githubusercontent.com/u/2384040?v=4" width="100;" alt="power721"/>
            <br />
            <sub><b>power721</b></sub>
        </a>
    </td></tr>
</table>
<!-- readme: contributors -end -->

## License

[MIT](LICENSE)
