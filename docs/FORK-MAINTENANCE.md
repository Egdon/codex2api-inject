# Fork maintenance (local implementation; remote activation requires confirmation)

This fork tracks the **latest non-draft, non-prerelease GitHub release** of
`james-6-23/codex2api`, not the moving upstream `main` branch. Fork patches are
preserved with Git merges. An upstream release already ancestral to fork `main`
is a successful no-op; no baseline merge is needed in that case.

## Confirm before any remote changes

These files only define automation. Do not push branches/tags, create PRs, set
secrets, change repository/package settings, or dispatch workflows until the
owner explicitly confirms. Once activated, the scheduled workflow is authorized
to push a sync branch and open a PR; it never merges that PR automatically.
No tests or remote writes were performed while implementing these files. The
checks described below run **in GitHub Actions**, not locally as part of setup.
For separately approved local Go work, the Go executable is under
`$HOME/.local/go/bin` (add that directory to PATH).

## Required repository setup

1. After approval, land these files on the fork's default branch, **main**. Enable
   GitHub Actions for the fork, including the scheduled workflow and required
   third-party actions. GitHub may disable schedules in inactive forks; check the
   Actions page. Schedules execute default-branch workflow code. Manual sync must
   select main; other branches are rejected.
2. Create repository Actions secret **UPSTREAM_SYNC_TOKEN**, a PAT belonging to a
   maintainer with access to this fork. Fine-grained repository permissions must
   include **Contents: read/write**, **Pull requests: read/write**, and
   **Workflows: read/write**. Upstream releases can change `.github/workflows`, so
   Contents permission alone is insufficient. For a classic PAT use the suitable
   repo/public_repo scope plus `workflow`, and authorize organization SSO if
   needed. The token needs read access to the public upstream release. Never put
   the token in the repository. Rotate it and limit access to trusted maintainers.
3. Before publishing, configure environment **patched-publish** with required
   reviewers (prevent self-review where available), and deployment ref rules
   allowing main and `patched-v*` only. Creating a named environment in YAML does
   **not** itself configure protection. Protect main and custom release tags;
   require review of workflow/script changes. Use a ruleset restricting custom
   tag creation to release maintainers. Never tag unreviewed PR commits.
4. Require all CI jobs before merging (frontend, govet lint, Go tests, PostgreSQL
   compatibility, every race shard, admin JavaScript, and Docker build). Ensure
   **admin/inject_page_js.test.mjs is tracked**, together with its Go source;
   local ignore rules must not silently omit it. Enable **merge commits** and
   merge sync PRs using **Create a merge commit**, not squash or rebase, so the
   upstream ancestry remains recorded. Do not enable auto-merge for sync PRs.
5. Permit Actions' `GITHUB_TOKEN` to write packages for publishing. Set the GHCR
   package visibility to **Public manually** after initial publication if public
   pulls are wanted; a public repository does not guarantee a public package.
   Check package linkage/Actions access if the package already exists.

## Upstream release sync

`upstream-sync.yml` runs daily at 06:23 UTC or manually on main. The script uses
fixed upstream/base values, validates release tag syntax, fetches full history,
and performs `git merge --no-ff` with a common-ancestor requirement. It never
allows unrelated histories, rebases main, force-pushes, or resets patches.

The PAT is used for **both branch push and PR creation**, so normal PR/push CI
can trigger (unlike events created using the default GITHUB_TOKEN). PAT-bearing
code comes from trusted default main, never a PR checkout. No upstream build or
test code is executed in the sync job, including after merging the candidate.
Review upstream workflow changes carefully before merging.

A conflict aborts the merge and fails **before any push**. Resolve it manually
on a dedicated branch, retain local patches, run CI, and request review. An
existing `sync/upstream-v…` branch or previous PR is left unchanged; reruns do
not reset review work or silently retry a closed proposal. If the push succeeded
but PR creation failed, open the PR for that existing branch manually after
confirmation. If main moves during creation, the script fails rather than
publishing a proposal that omits the new main commits. A release retag is not an
authorization to rewrite an existing review branch.

## CI and publication

`pr-check.yml` retains the existing checks and is reusable via `workflow_call`.
Its optional-at-event-time checkout expression uses the caller's required exact
candidate SHA for publication. Admin checks execute the tracked Node regression
suite against `admin/inject_page.go`, extract the embedded script and run
`node --check`. A separate Docker build runs with **push: false**. Ordinary
pull-request CI has read-only permissions and no publish secrets.

`docker-image.yml` only accepts custom **patched-v*** tag pushes (validated to
start with a numeric version), or a manual run selecting **main**. It never
publishes on upstream `v*` tags or regular main pushes. The candidate must be an
ancestor of current main, and the exact resolved SHA is checked by every reusable
CI job and checked out again for publication. Tags must point to reviewed main
history containing these guards: workflows triggered by tags are loaded from
that tagged commit. The reusable CI is from that same trusted workflow revision;
manual publishing loads main. There is no privileged `workflow_run` or
`pull_request_target` bridge to untrusted PR code.

The `build_version` input is passed via environment data, validated to a bounded
single-line allowlist, and then passed as a Docker build argument. It is never
interpolated into shell source. It changes the UI version, not publication tags.

After all CI succeeds and environment approval is granted, publication builds
linux/amd64 and linux/arm64 and pushes **ghcr.io/<owner>/<repo>:sha-<full SHA>**.
That tag is treated as immutable: reruns reuse its digest, never rebuild over it;
a rerun with a different display version still uses the first image. Restrict
other package writers, since GHCR does not enforce this convention for external
writers. Unexpected registry inspection failures fail closed.

All publication runs share one non-cancelling concurrency group. After the
immutable image exists, the workflow checks remote main HEAD **immediately before
promotion**. Only an equal candidate SHA may promote that **same digest** to
**patched-stable**. Older ancestor candidates can get an immutable image, but
cannot move stable backward after main advances. Digest identity is verified
after promotion; no rebuild is used. GitHub concurrency does not promise FIFO
order, and may replace pending runs; the HEAD check prevents stale promotion.
The Git ref check and GHCR update are not a cross-service atomic transaction;
main can advance just after the check, so stable can temporarily trail main.
There is deliberately **no latest tag**, upstream semver alias, or shadow publish.

The inherited `release.yml` remains restricted to
`github.repository == 'james-6-23/codex2api'`; it cannot create binary releases
on this fork's upstream `v*` tags. Preserve this guard during upstream merges.
Historical tags containing older workflows are not retroactively protected:
do not repush old tags or rerun historical unguarded publish/release runs. If
needed, disable inherited release automation in repository Actions settings.

## Review boundary

These protections assume main, custom release tags, approved workflow revisions,
and package writers are trusted. Review any proposed changes to these boundaries
before merging. Mutable action major-version references follow existing project
practice; commit-pin dependencies separately if required by your security policy.
Validate the first sync, CI, and publication runs in Actions after explicit remote
activation approval; local static review is not evidence those remote runs passed.
