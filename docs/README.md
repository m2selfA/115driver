# Documentation

Project documentation that does not need to live at the repository root is organized here.

## Release archive

### v0.2.0

The v0.2.0 release line retains its pre-release review and certification material as historical evidence:

- [`RELEASE_CHECKLIST.md`](release/v0.2.0/RELEASE_CHECKLIST.md) — operational RC/stable publication checklist used for the v0.2.0 line.
- [`RELEASE_NOTES_V0.2.0_RC1.md`](release/v0.2.0/RELEASE_NOTES_V0.2.0_RC1.md) — curated first-RC notes captured before publication.
- [`V0.2.0_RC1_INTEGRATION_AUDIT.md`](release/v0.2.0/V0.2.0_RC1_INTEGRATION_AUDIT.md) — compatibility and integration freeze audit.
- [`V0.2.0_RC1_COMMIT_MANIFEST.json`](release/v0.2.0/V0.2.0_RC1_COMMIT_MANIFEST.json) — frozen 402-path RC1 commit-boundary manifest.

These files are archived snapshots. Statements such as “the tag does not exist” describe the state at the time the snapshot was frozen; the corresponding releases have since been published.

## Repository-root convention

The root intentionally keeps only repository entry points and tooling-critical files such as `README.md`, `LICENSE`, `AGENTS.md`, `CLAUDE.md`, `Makefile`, Go module files, and release/build configuration. New long-form design, audit, experiment, or release-history documents should normally be added below `docs/`.
