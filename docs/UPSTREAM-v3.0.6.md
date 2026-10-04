# Upstream v3.0.6 integration and BPS retirement

## Provenance

- Fork base: `3644713ceff87fa21536a7374df6eb3919ae9e9e` (`patched-v1.0.9`, already includes v3.0.5).
- Official upstream release: `v3.0.6` / `9550e632f131310cafe9eabc466b6c4fed87edd8`.
- Integration uses a real Git merge; upstream ancestry is retained. Unreleased upstream main commits are not part of this update.
- This change does not create a new release tag, publish binary/container artifacts, deploy, or make real model requests.

## BPS is retired, not disabled behind another switch

Follow upstream's removal of the stopped Excel/Basispoints channel. Codex OAuth entry points use native routing; no BPS adapter, fallback route, pause-clear endpoint, health probe, replay/upload cache, global setting or account toggle is retained.

Fork-specific BPS eligibility checks, batch/quick controls, runtime settings migration and BPS-only ticket isolation are removed too. Existing settings/credential values can remain as inert historical data: no new migration rewrites or reactivates them. Removing BPS-specific conditions must not remove independent scheduler-generation, context cancellation, group/priority version, configuration epoch or compare-and-swap protections on surviving work.

Historical groups and priorities are not automatically restored or changed. Other channel admission constraints are not broadened to compensate for the retired channel.

## Historical source attribution remains available

The `bps` source identifier remains accepted for existing usage and quality-test records and their filters. It is a historical label, not an enabled channel. Existing records are not deleted, relabelled as Codex, or backfilled with guessed source values.

New request attribution follows actual dispatch. Independent Codex/other source tracing and last-dispatch behavior remain intact, including failed transports and admin connection/quality probes. Old BPS account flags do not determine the new source.

## Independent fork contracts

Preserve Antigravity channel admission, dedicated OAuth refresh and credential-publication concurrency checks, discovered wire-model IDs, API-key scope enforcement, and fail-closed experimental paths while incorporating upstream model/client updates. Batch groups use the fork's existing frozen cross-page selection UI; upstream's inline same-channel group creation is wired into that UI rather than adding a second competing modal. Creation is fenced against concurrent confirmation/cancellation.

Preserve fork build provenance and update protections: upstream version notifications must not offer an unsafe overwrite of a patched deployment. The running backend version supplied by v3.0.6 must coexist with the existing patched source/revision/upstream-base fields.

## Database and CI

Upstream adds `codex_client_version_cache` and `modeltrace_bank_override` for SQLite and PostgreSQL. Merge these schema changes alongside retained historical source columns/indexes. Legacy BPS settings columns, when already present, remain unread; fresh databases do not need the retired runtime configuration schema.

The PostgreSQL job now explicitly selects `TestPostgresBPSRetirementDatabase` rather than the removed `TestPostgresExcelBPSDatabase`. It must run real retirement/history compatibility assertions instead of silently matching no tests. Keep `TestPostgresTraceAndCapabilities`, `TestPostgresAstraPolicyPriority`, full tests and all race shards.

Local checks for this integration are compilation, type/build and syntax checks. Full Go/frontend tests, PostgreSQL compatibility, race shards and Docker build are gated in GitHub CI at the exact PR revision. The frontend CI job additionally runs the upstream `test:version` browser suite after Chromium installation, alongside the existing SVG regression suite; its fixtures retain the fork's source-bound update-plan contract. Do not report historical passing checks as current results.

## Publication and deployment protections

The merge does not change the patched release policy in [FORK-MAINTENANCE.md](FORK-MAINTENANCE.md):

- official upstream `v*` tags cannot publish this fork;
- custom releases use canonical `patched-vX.Y.Z` tags;
- exact candidate-SHA checks and `patched-publish` environment approval remain required;
- Docker immutable `sha-<full SHA>` and same-digest `patched-stable` promotion remain protected;
- no Docker `latest` alias is introduced;
- no Render/manual deploy dispatch is part of this merge.

After review and every required CI job passes, merge the PR with a merge commit, not squash or rebase. Publishing a subsequent patched release is a separate operation.
