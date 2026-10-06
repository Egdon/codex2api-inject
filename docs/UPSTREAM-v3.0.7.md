# v3.0.7 integration / patched-v1.1.0

## Scope and provenance

- Fork base: `ec9f2e8c0ef202956ff0f491a92219121e82001c` (`patched-v1.0.10`).
- Official stable upstream: `v3.0.7`, `910712b91f7c650252cb73d8bc5e6ea661dce006`.
- Preserve true Git merge ancestry. Do not incorporate unreleased upstream main.
- Target fork release: `patched-v1.1.0`; tag-driven frontend and Go build metadata retain `Source=patched`, exact revision and official upstream base.
- Artifact publication only; no server deployment, service startup or real model requests.

## Integration decisions

The PostgreSQL settings conflict is reconciled field-by-field with fork settings and historical source handling intact. SQLite/PostgreSQL both support the new `codex_unified_client_identity_enabled` (default false) and `show_upstream_model_mismatch` (default true) settings. Existing settings must survive round-trip updates and reopening, including installations retaining inert legacy BPS columns.

Accept upstream's optional per-account `keep_concurrency_on_degrade` behavior; do not automatically enable it or weaken banned-account, quota or premium usage-window guards. Accept maintenance-identity, Grok explicit-model and Antigravity stream-error fixes without restoring the retired BPS runtime.

Usage UI follows upstream in removing duplicate Turn State presentation from User-Agent cells while retaining fork independent audit columns and historical actual-source filters. Preserve patched/official update source isolation, expiring exact-tag installation plans, Antigravity OAuth compatibility and actual-dispatch attribution.

Historical BPS records are neither deleted nor relabelled. No automatic group/priority restoration or credential migration is introduced.

## Clean four-platform binary assembly

The prior release's later binaries reported `vcs.modified=true` because earlier archive staging created untracked files under the checkout before subsequent Go builds. The new assembler runs all Go output/packaging under external `$RUNNER_TEMP`. Only after every target passes does it stage the four archives and `SHA256SUMS.txt` under root `dist/` for the existing non-overwriting uploader.

Each compilation checks the tracked and untracked working tree. Each built artifact is read with `go version -m`, requiring the exact candidate revision, correct OS/architecture, CGO disabled and `vcs.modified=false`. Missing or unexpected provenance fails publication before a draft is created. There is no `-buildvcs=false`, ignored dirty-state shortcut, or rewritten provenance flag.

A synthetic test exercises all four targets, dirty/untracked checkout rejection, bad/missing build metadata, compiler-created source changes, invalid tags/revisions, temporary-directory isolation, and existing-output preservation. It runs inside ordinary CI using a fake Go executable; the real publisher performs the genuine cross-platform checks again.

## Checks and publication contract

Local compile/type/build/syntax checks precede PR submission. Full Go/frontend tests, PostgreSQL compatibility, all six race shards, version/SVG browser regressions, Docker build and security scans must pass the exact review commit. Replace neither required checks nor their assertions with skipped success.

Use a merge commit to land the reviewed PR, verify main CI and security, then create `patched-v1.1.0` on that exact main revision. Normal protected `patched-publish` approval remains required for both publishers.

Independently verify four binary archives and checksum asset, binary platform/revision/clean metadata, GitHub Latest, and both OCI platform configurations. `patched-stable` must identify the same OCI digest as `sha-<release SHA>`. Never overwrite a public release, asset or immutable SHA image to repair a failed attempt.
