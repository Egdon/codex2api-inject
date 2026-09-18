#!/usr/bin/env bash
# Shared release validation. Sourced only from reviewed candidate commits.
set -euo pipefail

canonical_patched_tag() {
  [[ "$1" =~ ^patched-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

revalidate_candidate() {
  local sha="$1" tag="${2:-}" remote_sha
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]]
  [[ $(git rev-parse 'HEAD^{commit}') == "$sha" ]]
  git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
  git merge-base --is-ancestor "$sha" refs/remotes/origin/main
  if [[ -n "$tag" ]]; then
    canonical_patched_tag "$tag"
    # Fetch the exact remote ref, including annotated tags, not a cached local tag.
    git fetch --no-tags origin "refs/tags/$tag"
    remote_sha=$(git rev-parse 'FETCH_HEAD^{commit}')
    [[ "$remote_sha" == "$sha" ]]
  fi
}

upstream_base() {
  local sha="$1" ref tag commit distance best='' best_distance=2147483647
  # Local v* names alone cannot prove official provenance. Use a separate namespace.
  if ! git fetch --no-tags https://github.com/james-6-23/codex2api.git \
      '+refs/tags/v*:refs/upstream-official-tags/v*' >&2; then
    printf 'unknown\n'
    return
  fi
  while IFS= read -r ref; do
    tag=${ref##*/}
    [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || continue
    commit=$(git rev-parse "$ref^{commit}" 2>/dev/null) || continue
    git merge-base --is-ancestor "$commit" "$sha" || continue
    distance=$(git rev-list --count "$commit..$sha")
    if (( distance < best_distance )); then
      best="$tag"
      best_distance="$distance"
    fi
  done < <(git for-each-ref --sort=refname --format='%(refname)' refs/upstream-official-tags/)
  printf '%s\n' "${best:-unknown}"
}
