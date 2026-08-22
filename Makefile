SHELL := /bin/sh

GO ?= go
PKG ?= ./...
DRIVER_PKG ?= ./pkg/driver
BIN_DIR ?= bin
CLI_MAIN ?= ./cmd/115driver
MCP_MAIN ?= ./cmd/115driver-mcp-server
CLI_BIN ?= $(BIN_DIR)/115driver
MCP_BIN ?= $(BIN_DIR)/115driver-mcp-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CLI_LDFLAGS ?= -s -w -X github.com/SheltonZhu/115driver/cli/cmd.version=$(VERSION)
MCP_LDFLAGS ?= -s -w -X github.com/SheltonZhu/115driver/mcp/server.version=$(VERSION)
ALIAS_REPAIR_CERT_COUNT ?= 10
ALIAS_REPAIR_INTERNAL_RUN ?= ^Test(RemoveOrphanReviewAliasesExact|ExactRepairRawPlanLocks)
ALIAS_REPAIR_CLI_RUN ?= ^Test(PlanCLISyncJournalAliasRepair|ReconcileCLISyncJournalAliasBatch|CLIAliasRepairBatchFailure|SyncJournalAliasBatch)
ALIAS_REPAIR_MCP_RUN ?= ^Test(PlanSyncJournalAliasRepair|ExecuteSyncJournalAliasRepair|AliasRepairExecutionError|SyncJournalAliasRepairBatchWire)
SYNC_SESSION_CERT_COUNT ?= 5
SYNC_SESSION_TRANSFER_RUN ?= ^Test(SessionIdentityV2|SessionStore|ImportLegacySession|TouchManagedSession|QuarantineCorruptLocation|SessionAdmin|SessionLock|OpportunisticGC|TransferTreeSession|ValidateTransferTreeSession)
SYNC_SESSION_EXEC_RUN ?= ^Test(BuildGraph|Execute|ValidateSafety|ExpectedSubtree|CompareSubtree)
SYNC_SESSION_GUARD_RUN ?= ^TestValidate(Remote|Local)Subtree
SYNC_SESSION_CLI_RUN ?= ^Test(PreparedMigration|SyncJournal(Migration|Migrate|RecoverMigrationBatch|DoctorDetectsInterruptedMigrationBatch|DoctorDoesNotMisclassifyActiveMigrationBatch|Resume|TwoRunResume|DestructivePhaseRequiresReconciliation|ExecutionPersistsPhase|ParallelExecution)|OpenSyncResumeJournal|OpenSyncRecoveryJournal|VerifySyncJournalResume|VerifyRecoveryRequiredJournal|ReconcileSyncJournalAfterReview)
SYNC_SESSION_MCP_RUN ?= ^Test(ExecuteSyncPlan|ReconcileSyncRecovery|DiagnoseSyncRecovery|InspectSyncJournal|ListSyncExecutions)
SYNC_SESSION_APP_RUN ?= ^Test(RunVersion|RunHelp|RunRejects|RunUsesReadOnlyCookieCheck|ResolveSessionScope|NewSyncJournalStore)
RELEASE_PACKAGING_RUN ?= ^Test(ResolveVersion|RootAndVersionCommandShareBuildVersion|RunVersionDoesNotRequireAuthentication|RunHelpDoesNotRequireAuthentication|ReleasePackagingContract)
RELEASE_GOOS ?= linux darwin windows
RELEASE_GOARCH ?= amd64 arm64
ARTIFACT_SMOKE_VERSION ?= 9.9.9-r11-artifact-smoke
DIST_DIR ?= dist
EXPECTED_ARTIFACT_VERSION ?=
RELEASE_PREFLIGHT_SMOKE_TAG ?= v9.9.9-rc.1
RELEASE_PREFLIGHT_SMOKE_SHA ?= 0123456789abcdef0123456789abcdef01234567
RC_COMMIT_MANIFEST ?= V0.2.0_RC1_COMMIT_MANIFEST.json
RC_COMMIT_MANIFEST ?= V0.2.0_RC1_COMMIT_MANIFEST.json
RC_COMMIT_LAYER ?=

.DEFAULT_GOAL := help

.PHONY: all
all: build ## Build all binaries

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format Go packages
	$(GO) fmt $(PKG)

.PHONY: tidy
tidy: ## Tidy Go modules
	$(GO) mod tidy

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: test
test: ## Run local/unit Go tests (live 115 integration requires explicit opt-in)
	$(GO) test $(PKG)

.PHONY: test-sync-journal-alias-cert
test-sync-journal-alias-cert: ## Repeat the crash/concurrency and CLI/MCP alias-repair release matrix
	$(GO) test -count=$(ALIAS_REPAIR_CERT_COUNT) ./internal/syncjournal -run '$(ALIAS_REPAIR_INTERNAL_RUN)'
	$(GO) test -count=$(ALIAS_REPAIR_CERT_COUNT) ./cli/cmd -run '$(ALIAS_REPAIR_CLI_RUN)'
	$(GO) test -count=$(ALIAS_REPAIR_CERT_COUNT) ./mcp/server/tools -run '$(ALIAS_REPAIR_MCP_RUN)'

.PHONY: test-sync-journal-alias-race
test-sync-journal-alias-race: ## Run the same core alias-repair matrix under the Go race detector (requires CGO/race support)
	CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/syncjournal -run '$(ALIAS_REPAIR_INTERNAL_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./cli/cmd -run '$(ALIAS_REPAIR_CLI_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./mcp/server/tools -run '$(ALIAS_REPAIR_MCP_RUN)'

.PHONY: test-release-entrypoints
test-release-entrypoints: ## Install both public commands into a temporary GOBIN and smoke version/help without authentication
	@set -eu; tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; exe="$$(go env GOEXE)"; \
	GOBIN="$$tmp" CGO_ENABLED=0 $(GO) install -trimpath -ldflags '-s -w -X github.com/SheltonZhu/115driver/cli/cmd.version=$(ARTIFACT_SMOKE_VERSION)' $(CLI_MAIN); \
	GOBIN="$$tmp" CGO_ENABLED=0 $(GO) install -trimpath -ldflags '-s -w -X github.com/SheltonZhu/115driver/mcp/server.version=$(ARTIFACT_SMOKE_VERSION)' $(MCP_MAIN); \
	cli="$$tmp/115driver$$exe"; mcp="$$tmp/115driver-mcp-server$$exe"; \
	test "$$($$cli --version)" = "115driver version $(ARTIFACT_SMOKE_VERSION)"; \
	test "$$($$cli version)" = "115driver version $(ARTIFACT_SMOKE_VERSION)"; \
	test "$$($$mcp --version)" = "115driver-mcp-server $(ARTIFACT_SMOKE_VERSION)"; \
	$$cli --help >/dev/null; \
	$$mcp --help >/dev/null 2>&1

.PHONY: test-release-notes-cli
test-release-notes-cli: ## Exercise the release-notes command end to end against a temporary tagged Git history
	@set -eu; tmp="$$(mktemp -d)"; trap 'cd / && rm -rf "$$tmp"' EXIT; exe="$$(go env GOEXE)"; \
	$(GO) build -trimpath -o "$$tmp/release-notes$$exe" ./cmd/release-notes; \
	repo="$$tmp/repo"; mkdir -p "$$repo"; cd "$$repo"; git init -q; \
	git config core.autocrlf false; git config user.name 115driver-release-smoke; git config user.email release-smoke@example.invalid; \
	printf 'seed\n' > release.txt; git add release.txt; git commit -qm 'feat(release): seed artifact notes'; git tag v0.0.1; \
	printf 'fix\n' >> release.txt; git add release.txt; git commit -qm 'fix(release): smoke artifact notes'; git tag v0.0.2; \
	notes="$$($$tmp/release-notes$$exe -tag v0.0.2 -repo-url https://example.invalid/115driver)"; \
	printf '%s\n' "$$notes" | grep -F 'compare/v0.0.1...v0.0.2' >/dev/null; \
	printf '%s\n' "$$notes" | grep -F 'Bug Fixes' >/dev/null; \
	printf '%s\n' "$$notes" | grep -F '**release:** smoke artifact notes' >/dev/null

.PHONY: test-release-ops
test-release-ops: ## Verify release preflight/state semantics and the non-mutating CLI projection
	$(GO) test -count=1 ./internal/releaseops
	@set -eu; tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; printf '[{"id":1,"tag_name":"v9.9.8","draft":false,"prerelease":false}]\n' > "$$tmp/releases.json"; \
	$(GO) run ./cmd/release-preflight -tag '$(RELEASE_PREFLIGHT_SMOKE_TAG)' -expected-sha '$(RELEASE_PREFLIGHT_SMOKE_SHA)' -releases-file "$$tmp/releases.json" >/dev/null; \
	for state in absent draft published; do \
		$(GO) run ./cmd/release-preflight -tag '$(RELEASE_PREFLIGHT_SMOKE_TAG)' -expected-sha '$(RELEASE_PREFLIGHT_SMOKE_SHA)' -simulate-state "$$state" -releases-file "$$tmp/releases.json" >/dev/null; \
	done

.PHONY: verify-rc-commit-boundary
verify-rc-commit-boundary: ## Verify the frozen v0.2.0-rc.1 path set and dependency-safe commit layers
	$(GO) run ./cmd/release-boundary-check -manifest '$(RC_COMMIT_MANIFEST)'

.PHONY: verify-rc-index-empty
verify-rc-index-empty: ## Require that no RC changes are staged in the Git index
	$(GO) run ./cmd/release-boundary-check -manifest '$(RC_COMMIT_MANIFEST)' -verify-index-layer empty

.PHONY: verify-rc-staged-layer
verify-rc-staged-layer: ## Require that the Git index contains exactly RC_COMMIT_LAYER from the frozen commit manifest
	@test -n "$(RC_COMMIT_LAYER)" || { echo "RC_COMMIT_LAYER is required"; exit 1; }
	$(GO) run ./cmd/release-boundary-check -manifest '$(RC_COMMIT_MANIFEST)' -verify-index-layer '$(RC_COMMIT_LAYER)'

.PHONY: verify-release-artifacts
verify-release-artifacts: ## Verify GoReleaser archives, SPDX SBOMs, checksum manifests, contents, version, and host binary version/help output
	$(GO) run ./cmd/release-artifact-check -dist '$(DIST_DIR)' -expected-version '$(EXPECTED_ARTIFACT_VERSION)'

.PHONY: test-release-packaging
test-release-packaging: test-release-entrypoints test-release-notes-cli test-release-ops verify-rc-commit-boundary ## Verify release operations, commit boundary, metadata, and both entry points for every GoReleaser OS/arch target
	$(GO) test -count=1 ./internal/releaseartifact ./internal/buildinfo ./cli/cmd ./internal/mcpapp -run '$(RELEASE_PACKAGING_RUN)|^TestVerify'
	@set -eu; tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	for os in $(RELEASE_GOOS); do \
		for arch in $(RELEASE_GOARCH); do \
			ext=""; if [ "$$os" = windows ]; then ext=".exe"; fi; \
			echo "release cross-build $$os/$$arch cli+mcp"; \
			CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO) build -trimpath -ldflags '$(CLI_LDFLAGS)' -o "$$tmp/115driver-$$os-$$arch$$ext" $(CLI_MAIN); \
			CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" $(GO) build -trimpath -ldflags '$(MCP_LDFLAGS)' -o "$$tmp/115driver-mcp-server-$$os-$$arch$$ext" $(MCP_MAIN); \
		done; \
	done

.PHONY: test-sync-session-cert
test-sync-session-cert: test-sync-journal-alias-cert ## Repeat the full sync/session release matrix, then certify release packaging
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./internal/transfer -run '$(SYNC_SESSION_TRANSFER_RUN)'
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./internal/syncexec -run '$(SYNC_SESSION_EXEC_RUN)'
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./internal/syncguard -run '$(SYNC_SESSION_GUARD_RUN)'
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./cli/cmd -run '$(SYNC_SESSION_CLI_RUN)'
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./mcp/server/tools -run '$(SYNC_SESSION_MCP_RUN)'
	$(GO) test -count=$(SYNC_SESSION_CERT_COUNT) ./internal/mcpapp -run '$(SYNC_SESSION_APP_RUN)'
	$(MAKE) test-release-packaging

.PHONY: test-sync-session-race
test-sync-session-race: test-sync-journal-alias-race ## Run the full sync/session certification core under the Go race detector
	CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/transfer -run '$(SYNC_SESSION_TRANSFER_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/syncexec -run '$(SYNC_SESSION_EXEC_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/syncguard -run '$(SYNC_SESSION_GUARD_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./cli/cmd -run '$(SYNC_SESSION_CLI_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./mcp/server/tools -run '$(SYNC_SESSION_MCP_RUN)'
	CGO_ENABLED=1 $(GO) test -race -count=1 ./internal/mcpapp -run '$(SYNC_SESSION_APP_RUN)'

.PHONY: release-ready
release-ready: ## Run the complete cross-platform non-race gate: module verification, tests, vet, sync/session certification, and entry-point build
	$(GO) mod verify
	RUN_115_INTEGRATION=0 RUN_115_DESTRUCTIVE_INTEGRATION=0 $(GO) test -count=1 $(PKG)
	$(GO) vet $(PKG)
	$(MAKE) test-sync-session-cert
	CGO_ENABLED=0 $(GO) build $(CLI_MAIN) $(MCP_MAIN)

.PHONY: release-ready-race
release-ready-race: release-ready ## Add LF-stable module tidy verification and the mandatory race-detector sync/session matrix
	$(GO) mod tidy -diff
	$(MAKE) test-sync-session-race

.PHONY: test-integration-readonly
test-integration-readonly: ## Run read-only live pkg/driver integration tests (requires COOKIE)
	@test -n "$(COOKIE)" || { echo "COOKIE is required for integration tests"; exit 1; }
	RUN_115_INTEGRATION=1 RUN_115_DESTRUCTIVE_INTEGRATION=0 COOKIE="$(COOKIE)" $(GO) test -count=1 -run '^TestReadOnlyIntegrationSmoke$$' $(DRIVER_PKG)

.PHONY: test-integration
test-integration: ## Run all live pkg/driver integration tests, including destructive ones (requires COOKIE)
	@test -n "$(COOKIE)" || { echo "COOKIE is required for integration tests"; exit 1; }
	RUN_115_INTEGRATION=1 RUN_115_DESTRUCTIVE_INTEGRATION=1 COOKIE="$(COOKIE)" $(GO) test -count=1 $(DRIVER_PKG)

.PHONY: test-all
test-all: test test-integration ## Run unit and integration tests

.PHONY: build-cli
build-cli: $(BIN_DIR) ## Build the CLI binary
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(CLI_LDFLAGS)' -o $(CLI_BIN) $(CLI_MAIN)

.PHONY: build-mcp
build-mcp: $(BIN_DIR) ## Build the MCP server binary
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(MCP_LDFLAGS)' -o $(MCP_BIN) $(MCP_MAIN)

.PHONY: build
build: build-cli build-mcp ## Build all binaries into bin/

.PHONY: install-cli
install-cli: ## Install the CLI binary with go install
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags '$(CLI_LDFLAGS)' $(CLI_MAIN)

.PHONY: install-mcp
install-mcp: ## Install the MCP server binary with the current source version
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags '$(MCP_LDFLAGS)' $(MCP_MAIN)

.PHONY: check
check: vet test build ## Run the standard verification suite

.PHONY: pre-commit
pre-commit: ## Run all pre-commit hooks
	pre-commit run --all-files

.PHONY: clean
clean: ## Remove local build artifacts
	rm -rf $(BIN_DIR)

$(BIN_DIR):
	mkdir -p $(BIN_DIR)
