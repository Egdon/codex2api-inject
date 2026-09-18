#!/usr/bin/env bash
# Run only from the trusted main checkout in upstream-sync.yml.
set -euo pipefail
: "${GH_TOKEN:?UPSTREAM_SYNC_TOKEN is required}"
: "${GH_REPO:?Fork repository is required}"
[[ "$GH_REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || exit 1
[[ "$GH_REPO" != james-6-23/codex2api ]] || exit 1

# No event-controlled repository, base branch, refspec, or command interpolation.
release=$(gh api repos/james-6-23/codex2api/releases/latest)
tag=$(jq -er 'select(.draft == false and .prerelease == false) | .tag_name' <<<"$release")
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || {
  printf 'Unsupported upstream release tag: %s\n' "$tag" >&2
  exit 1
}
git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
git fetch --no-tags https://github.com/james-6-23/codex2api.git "refs/tags/$tag"
upstream_sha=$(git rev-parse 'FETCH_HEAD^{commit}')
if git merge-base --is-ancestor "$upstream_sha" refs/remotes/origin/main; then
  printf '%s is already integrated; nothing to do.\n' "$tag"
  exit 0
fi
# Fail on disconnected histories; never use --allow-unrelated-histories.
git merge-base refs/remotes/origin/main "$upstream_sha" >/dev/null
branch="sync/upstream-$tag"
# Do not reset or append to a branch under review, even if upstream retags.
existing=$(git ls-remote --heads origin "refs/heads/$branch")
if [[ -n "$existing" ]]; then
  printf 'Review branch %s already exists; leaving it unchanged.\n' "$branch"
  printf 'If PR creation previously failed, open a PR for this branch manually.\n'
  exit 0
fi
# Also leave previously closed PRs alone until a maintainer explicitly intervenes.
prs=$(gh pr list --repo "$GH_REPO" --head "$branch" --state all --json number --jq 'length')
if [[ "$prs" != 0 ]]; then
  printf 'A PR already exists for %s; leaving review history unchanged.\n' "$branch"
  exit 0
fi
git switch --create "$branch" refs/remotes/origin/main
git config user.name 'upstream-sync[bot]'
git config user.email 'upstream-sync[bot]@users.noreply.github.com'
if ! git -c core.hooksPath=/dev/null merge --no-ff --no-edit "$upstream_sha"; then
  git merge --abort
  printf 'Merge conflict: no branch was pushed. Resolve manually preserving fork patches.\n' >&2
  exit 1
fi
# Refuse to publish a stale proposal if main moved during the merge.
git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
git merge-base --is-ancestor refs/remotes/origin/main HEAD
# Ordinary push only: a concurrently created review branch is never force-reset.
git push origin "HEAD:refs/heads/$branch"
gh pr create --repo "$GH_REPO" --base main --head "$branch" \
  --title "Merge upstream $tag (preserve fork patches)" \
  --body "Merges stable upstream release $tag ($upstream_sha) using a real Git merge. Review fork patches and workflow changes, require all CI checks, then merge with a merge commit (not squash/rebase). No auto-merge is enabled."
