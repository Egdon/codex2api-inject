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

`upstream-sync.yml` runs daily at 19:23 UTC (03:23 the following day in
Asia/Shanghai, UTC+8), or manually on main. GitHub schedules may be delayed. The script uses
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

`docker-image.yml` only accepts canonical **patched-vMAJOR.MINOR.PATCH** tag
pushes (three numeric components, no leading zero except zero itself), or a manual
run selecting **main**, in exactly `Egdon/codex2api-inject`. It never
publishes on upstream `v*` tags or regular main pushes. The candidate must be an
ancestor of current main, and the exact resolved SHA is checked by every reusable
CI job and checked out again for publication. Tags must point to reviewed main
history containing these guards: workflows triggered by tags are loaded from
that tagged commit. The reusable CI is from that same trusted workflow revision;
manual publishing loads main. There is no privileged `workflow_run` or
`pull_request_target` bridge to untrusted PR code.

The manual `build_version` input is environment data, never interpolated into
shell source. If supplied, it must be a canonical patched tag already resolving
to the selected main SHA. If empty, the version is `dev-<full SHA>`, never `main`
or an official upstream version. Tag pushes use the exact tag. Docker passes
`BUILD_SOURCE=patched`, `BUILD_UPSTREAM_BASE` and `BUILD_REVISION` alongside the
version; frontend and binary use the same version. The Dockerfile defaults to
`dev`, `patched`, `unknown`, `unknown` for honest standalone builds.

After all CI succeeds and environment approval is granted, publication builds
linux/amd64 and linux/arm64 and pushes **ghcr.io/<owner>/<repo>:sha-<full SHA>**.
That tag is treated as immutable: reruns reuse its digest, never rebuild over it.
Reuse requires both platform images' metadata labels to match the requested
version, source, upstream base and revision; missing/mismatched labels fail closed.
A dev image already published at that SHA cannot become a differently versioned
release image: use a new reviewed commit, not an overwrite. Restrict
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

## Fork binary releases and exact-tag trust contract

`patched-release.yml` is restricted to exactly **Egdon/codex2api-inject**. It
accepts patched tag pushes or a manual run on main with an existing tag. Valid
examples include `patched-v1.0.0` and `patched-v0.0.0`; suffixes, leading zeros,
missing components, and upstream `v*` tags are rejected. The workflow resolves
and peels the remote tag once to a full immutable commit SHA, verifies main
ancestry, and calls all reusable CI jobs with that exact SHA. Only the publish
job has `contents: write`, behind the protected **patched-publish** environment.
Permit that token permission in repository settings after explicit approval.

After approval, the job revalidates main ancestry and remote tag SHA, builds the
frontend with `VITE_APP_VERSION` equal to the exact tag, and embeds these Go
`internal/version` string variables with ldflags:

- `Version`: exact patched tag, never stripped to upstream semver.
- `Source`: `patched`.
- `Revision`: full tested commit SHA.
- `UpstreamBase`: nearest ancestral canonical official `vX.Y.Z` tag, fetched
  directly from `james-6-23/codex2api` into an isolated ref namespace. Distance is
  the number of commits in `tag..candidate`; ties use lexical ref order. Local
  similarly named tags are not proof. If provenance cannot be established, use
  `unknown` rather than inventing an upstream version.

The draft contains exactly four archives and `SHA256SUMS.txt`. For example,
`codex2api_patched-v1.0.0_linux_amd64.tar.gz` contains `codex2api`, `.env.example`,
and `README.md` at its root. Targets are linux/darwin × amd64/arm64. Windows
installation is unsupported; this workflow does not publish Windows packages.
Archive names retain the **complete exact patched tag**. The checksum manifest
uses standard SHA-256 lines for these exact archive basenames.

No existing release (public or draft) or asset is overwritten. Upload does not
use `--clobber`. Before publication, all five uploaded assets must have exactly
the expected names, uploaded state, sizes and GitHub SHA-256 digests matching
local bytes. Missing digests fail closed. Remote tag SHA and main ancestry are
checked again as the last Git operation before making the draft public. Latest
selection is explicit and numeric-safe: an older canonical version is published
with `make_latest=false`, so an older manual release cannot demote latest.

If assembly/upload/validation fails, the draft remains private and the next run
refuses it. After separately approved manual inspection, delete **only that
failed draft**, keep the tag unchanged, and rerun from the start. Automatic resume
is intentionally unsupported. Never delete or recreate a public release to retry;
publish a new canonical version instead. Protect release assets and tag update/
deletion with rulesets and restrict other release writers. Git checks and GitHub
release publication are not atomic; protections against concurrent retagging or
out-of-band publication are part of the trust boundary.

Updater integration must bind repository, exact canonical tag, selected platform
archive basename, and checksum manifest from **that same tag release**. It must
verify SHA-256 before extraction/replacement, never substitute `latest` download
URLs, upstream assets, or checksums from another release. The manifest is an
integrity check under the trusted repository/publisher boundary, not an independent
signature: a compromised writer can replace both archive and manifest. Keep
frontend/backend strict-tag parsing and metadata in agreement with this contract.
GitHub's moving latest release pointer is discovery only, not artifact identity.

## Review boundary

These protections assume main, custom release tags, approved workflow revisions,
and package writers are trusted. Review any proposed changes to these boundaries
before merging. Mutable action major-version references follow existing project
practice; commit-pin dependencies separately if required by your security policy.
Validate the first sync, CI, and publication runs in Actions after explicit remote
activation approval; local static review is not evidence those remote runs passed.
