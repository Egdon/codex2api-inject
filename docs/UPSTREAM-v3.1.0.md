# Stable v3.1.0 integration / patched-v1.1.2

## Scope

Merge official v3.1.0 (`b04eb97d6360cd6e6c5a76cdc0934313b4c43690`) from fork main `942e7fe789ecfffb0b49cf76eb21034ad125263e`. This includes v3.0.9's page extraction and hourly usage aggregation, not just v3.1.0's Claude fixes. Scheduled sync 38332220770 stopped on genuine conflicts before push; authentication and fetch succeeded. Preserve real merge ancestry and abort-on-conflict policy.

After reviewed main and exact-revision CI/security pass, publish patched-v1.1.2 using existing protected workflows. No server deployment, application startup or real-model traffic.

## Merge decisions

- Keep the fork `upstream_source` insert/scan/filter and historical records while accepting upstream RETURNING/log-ID processing for hourly aggregate updates. Source is not a dimension in the upstream hourly table, so source-filtered queries must continue to use raw rows via `HasDimensionFilter`; do not manufacture source attribution in aggregates.
- Accept extracted Antigravity page/modal/row components, statistics, filters, detail drawer and new batch-model/lock/reset functionality. Preserve one minimal cross-page selection map, frozen targets and the existing batch controller for overlapping test/refresh/enable/disable/groups/proxy/export/delete operations. Share synchronous busy fences with the new page actions and keep selection when aggregate results cannot identify individual successes.
- Preserve historical source badges in the operation-results modal and read-only minimal account metadata. Keep existing cooldown audit details in the new provider panel and group creation in the existing batch UI.
- Merge locale namespaces rather than replacing fork selection/batch messages with the new upstream settings. Export filename fallback follows upstream's new default JSON output.
- Integrate Claude lowercase tool schemas, model-specific tool-choice compatibility, JSON-mode response handling and account-scope-aware direct model routing. Explicit mappings and API Key scope restrictions retain priority. BPS remains retired.
- Existing patched update plans, clean four-platform binary assembler, immutable-image and environment approval safeguards stay intact.

## Verification

Local frontend typecheck/build and Go compile-only are required, followed by GitHub full tests, PostgreSQL compatibility, all race shards, browser checks, Docker build and security scanning. Added regressions cover source-filtered statistics after hourly rebuilding and frontend page extraction/selection/busy/result contracts. PostgreSQL source coverage is wired through the existing required retirement/history integration entrypoint.

Before publication, verify main CI/security and current official target, then re-run upstream-sync for the already-integrated no-op. Release assets must preserve exact tag/revision/upstream-base and clean VCS metadata; independently verify all four archives/checksums and both OCI platform configs. Do not overwrite old releases or immutable images.
