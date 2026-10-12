# Upstream v3.0.8 synchronization repair

## Failure and provenance

The scheduled synchronization run `38182533349` fetched the official v3.0.8 release successfully, then stopped before pushing because of conflicts in `database/postgres.go`, `proxy/wsrelay/executor.go`, and `proxy/wsrelay/manager.go`. No authentication or publication-policy change is needed. Automatic abort-on-conflict remains enabled.

- Fork base: `6ecad23ce1dbb144667b1346835ea5a35ba19907` (`patched-v1.1.0`).
- Official upstream v3.0.8: `51e4f1507d6d7dd7c95a01b6690da8b352311dbd`.
- Merge with a real merge commit, not squash/rebase. No unreleased upstream main commits.
- This repair does not create release tags, publish artifacts, deploy servers or make real model requests.

## Conflict resolution

### Settings and usage queries

Retain the fork transaction wrapper for system settings and add upstream's downstream-chat keepalive budget at its actual SQL parameter position. Preserve existing prompt-setting update guards and all independent settings.

The optimized paginated usage query uses upstream's ID-first page selection and separate count query rather than the former window count. Keep the historical `upstream_source` SELECT/Scan pair in the optimized projection, and retain its filters for pages, exports and statistics. Do not rewrite or drop historical data.

Extend the existing SQLite/PostgreSQL legacy-restart regression to cover the new default 8, missing-column upgrade, explicit 0/32/8 chat and stateless budgets, and historical-source pagination after migration. The existing `TestPostgresBPSRetirementDatabase` CI selector executes these checks.

### WebSocket execution and connection lifecycle

Use upstream's new `connectionDialRequest` and extracted `connection_dial.go` implementation; do not keep the obsolete duplicate `Manager.createConnection` in manager.go. Place the fork actual-dispatch trace immediately before the relocated DialContext, after local proxy setup has succeeded.

Preserve fork context-aware write-boundary attribution while accepting upstream's `ResponsesContinuationLostError` on failed sends associated with `previous_response_id`. Leave upstream recovery limits, complete-history requirements, compatibility scopes, capacity accounting, LRU retention and in-flight protection intact.

Adapt the fork failed-dial regression to the new request structure and add a local-invalid-proxy regression proving it does not fabricate or overwrite prior actual source evidence. Existing native version-preparation and write-boundary regressions remain enabled.

## Other integration checks

- Go/Docker builder follows upstream Go 1.26.9. Typed atomic field changes must compile with existing fork code; upstream architecture-source regressions remain active.
- Keep the fork independent source/audit UI, Antigravity compatibility, and guarded updater/release behavior. BPS runtime remains removed.
- Add Traditional Chinese labels for the new chat retention setting; do not modify its runtime default.
- Preserve the clean four-platform publication assembler and immutable-image protections from patched-v1.1.0.

## Verification gates

Build frontend before Go compilation so embed artifacts are not replaced during compilation. Local checks cover type/build/compile and syntax. Full Go tests, PostgreSQL, all race shards, frontend/browser regressions, Docker build and security scanning run in GitHub CI on the exact reviewed revision.

After all required checks pass, merge the repair PR with a merge commit. Confirm v3.0.8 ancestry on main and manually run the existing upstream-sync workflow once to verify its already-integrated no-op path. Do not loosen failure handling to mask future real conflicts, rewrite old failed run records, or automatically publish a new release.
