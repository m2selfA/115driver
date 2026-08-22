# Release Candidate Checklist

This checklist is the operational companion to the automated release gates. It does not replace `make release-ready-race`, the GoReleaser artifact verifier, GitHub draft round-trip verification, or provenance/SBOM attestation checks.

## Before creating a tag

- [ ] Choose the version bump deliberately, then use a SemVer prerelease tag. The current post-`v0.1.4` work adds broad CLI batch/sync/session recovery and MCP planning/recovery surfaces, so its intended candidate line is the minor bump `v0.2.0-rc.1`; reserve a patch bump such as `v0.1.5-rc.1` for a bug-fix-only scope.
- [ ] Confirm the intended release commit is the exact commit selected for the manual `release-dry-run` workflow.
- [ ] Run the manual `release-dry-run` workflow with the proposed tag. It has `contents: read` only and must not create, upload, edit, publish, or attest a GitHub release.
- [ ] Review the real GitHub release preflight result. `absent` is expected for a new RC; an existing `draft` is repairable only when it contains no unknown asset names; `published` must already contain the exact thirteen assets and is verify-only.
- [ ] For an `absent` or `draft` candidate, require `latest_published_tag` to be the highest published SemVer and require the proposed tag to have strictly higher SemVer precedence. Build metadata alone does not advance the release lineage.
- [ ] If a stable tag follows an active prerelease line, require `promotion_from` to name the latest prerelease, require the stable tag to use exactly the same `MAJOR.MINOR.PATCH` core, and require the stable candidate SHA to equal that prerelease tag's commit exactly. Any commit after the latest RC requires another prerelease (for example `rc.2`) before stable promotion.
- [ ] Review all three simulated state plans (`absent`, partial `draft`, `published`) emitted by the dry-run workflow.
- [ ] Confirm the dry-run candidate is the proposed final version, not a snapshot version, and that `make verify-release-artifacts` reports six archives, six SPDX SBOMs, host smoke, and install smoke.
- [ ] Review generated release notes and the previous-tag compare range.
- [ ] Review `V0.2.0_RC1_INTEGRATION_AUDIT.md` and confirm the candidate still matches its frozen CLI/MCP/Go API and persisted-state compatibility boundary.
- [ ] Review `RELEASE_NOTES_V0.2.0_RC1.md` together with generated Git-history notes; reconcile discrepancies before tagging rather than silently publishing either source as authoritative.
- [ ] Run `make verify-rc-commit-boundary` and require the exact 402-path manifest plus the five frozen layer counts/hashes in `V0.2.0_RC1_COMMIT_MANIFEST.json`.
- [ ] Before materializing commits, run `make verify-rc-index-empty`. Generate the layer pathspec from `release-boundary-check -print-layer <layer> -print-layer-nul`, stage only that NUL-delimited pathspec with Git, then run `make verify-rc-staged-layer RC_COMMIT_LAYER=<layer>` before reviewing the cached diff or committing it.
- [ ] Preserve the tested order and subjects: `driver` -> `core` -> `cli` -> `mcp` -> `release`. After each commit require `make verify-rc-index-empty`, rerun `make verify-rc-commit-boundary`, and do not silently move a path between layers to make a partial commit convenient.
- [ ] For every staged layer, run `git diff --cached --check`, review `git diff --cached --stat` and `git diff --cached --summary`, and explicitly account for additions/modifications/deletions before committing. The checker verifies path ownership, not semantic correctness of the diff itself.
- [ ] After the five commits are complete, require a clean worktree, rerun the full release gate, and confirm Git-history notes retain the five intended feature scopes before creating the real RC tag.

## Tag and publication

- [ ] Create the tag on the reviewed commit and push that tag without moving or recreating it.
- [ ] The tag workflow must resolve the tag to `GITHUB_SHA` and classify the GitHub release before any release mutation.
- [ ] RC/beta/alpha-style SemVer prerelease tags must be marked GitHub prereleases. Stable `vMAJOR.MINOR.PATCH` tags must not be marked prereleases.
- [ ] For a new release, allow the workflow to create only a draft. For an interrupted run, rerun the same tag workflow and let the draft-repair path clobber only the thirteen expected asset names.
- [ ] Do not manually add extra draft assets. Unknown asset names fail the preflight/asset-count gates and require review rather than automatic deletion.
- [ ] Confirm the remote draft round trip downloads six archives, six SPDX SBOMs, and `checksums.txt`, and that all twelve manifest digests verify before publication.
- [ ] Confirm all twelve generated files have provenance, `checksums.txt` has its own provenance, and all six archives have SPDX 2.3 SBOM attestations bound to the exact repository/workflow/source commit/tag ref.
- [ ] Publication must occur only after the remote byte round trip succeeds.

## After publication

- [ ] Confirm the release is no longer a draft, has the expected prerelease flag, has the exact tag name, and has exactly thirteen assets.
- [ ] Download a release archive and run `gh attestation verify <artifact> -R OWNER/REPO`, using the exact GitHub repository from which that release was downloaded (the workflow attests against `$GITHUB_REPOSITORY`).
- [ ] Rerun the same tag workflow once if operationally appropriate: a published release must take the read-only verification path and must not rebuild, attest, upload, edit, or republish anything.
- [ ] Record any operational discrepancy before promoting a release candidate to a stable tag.

## RC soak and stable promotion

- [ ] From the public GitHub Release, download at least one host-platform archive plus `checksums.txt`, verify the archive digest, unpack it outside the repository, and run both public executables with `--version` and `--help` without authentication.
- [ ] Run `gh attestation verify` against the downloaded public archive using the repository that published it; do not rely only on the pre-publication `dist/` verification.
- [ ] Rerun the published RC's tag workflow once. Require `Verify existing published release` to succeed and require the toolchain, readiness, GoReleaser, attestation, draft staging, upload, and publication steps to be skipped. Compare the release/asset metadata before and after the rerun and require it to remain unchanged.
- [ ] Treat the latest published prerelease commit as the stable source of truth. The `release-dry-run` and tag workflows enforce `git rev-list -n 1 "$promotion_from" == candidate SHA`; if `main` has moved after the RC for code, tests, documentation, or release automation, publish the next RC first rather than promoting the changed tree directly to stable.
- [ ] Before stable promotion, require no unresolved RC blocker from public installation smoke, published-rerun verification, CI/race certification, or user-reported regressions. A stable tag changes the embedded version and release channel only; it must not be used to smuggle unsoaked source changes past the RC line.
