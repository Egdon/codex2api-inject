#!/usr/bin/env bash
set -euo pipefail
source .github/scripts/patched-release-common.sh
[[ "$GH_REPO" == Egdon/codex2api-inject ]]
canonical_patched_tag "$RELEASE_TAG"
revalidate_candidate "$CANDIDATE_SHA" "$RELEASE_TAG"

# Fail closed on every error except a confirmed missing release. Never resume or
# replace assets automatically, even on drafts: maintainers must delete the draft.
error_file=$(mktemp)
trap 'rm -f "$error_file"' EXIT
if gh api "repos/$GH_REPO/releases/tags/$RELEASE_TAG" >/dev/null 2>"$error_file"; then
  printf 'Release already exists; refusing to modify it (including drafts).\n' >&2
  exit 1
elif ! grep -Fq '(HTTP 404)' "$error_file"; then
  printf 'Cannot establish release absence; refusing publication.\n' >&2
  exit 1
fi

release_id=$(gh api --method POST "repos/$GH_REPO/releases" \
  -f tag_name="$RELEASE_TAG" -f target_commitish="$CANDIDATE_SHA" \
  -f name="$RELEASE_TAG" -F draft=true -F prerelease=false \
  -f body="Fork binary release. Source: patched. Revision: $CANDIDATE_SHA. Upstream base: $UPSTREAM_BASE. Verify the exact-tag SHA256SUMS.txt before installing." \
  --jq '.id')
[[ "$release_id" =~ ^[0-9]+$ ]]
gh release upload "$RELEASE_TAG" dist/codex2api_*.tar.gz dist/SHA256SUMS.txt --repo "$GH_REPO"
# Verify the complete name/size/SHA-256 map, including the checksum file itself.
# GitHub's asset digest is required: absent digest is not proof of integrity.
gh api "repos/$GH_REPO/releases/$release_id" > "$RUNNER_TEMP/patched-draft.json"
python3 - "$RUNNER_TEMP/patched-draft.json" <<'PY'
import hashlib, json, os, pathlib, sys
release = json.load(open(sys.argv[1]))
tag = os.environ['RELEASE_TAG']
assert release['draft'] is True and release['tag_name'] == tag
names = {f'codex2api_{tag}_{target}.tar.gz' for target in
         ('linux_amd64', 'linux_arm64', 'darwin_amd64', 'darwin_arm64')}
names.add('SHA256SUMS.txt')
assets = release['assets']
assert len(assets) == 5 and {a['name'] for a in assets} == names
for asset in assets:
    data = (pathlib.Path('dist') / asset['name']).read_bytes()
    assert asset['state'] == 'uploaded'
    assert asset['size'] == len(data)
    assert asset.get('digest') == 'sha256:' + hashlib.sha256(data).hexdigest()
PY
# Choose latest explicitly; arbitrary-sized numeric components are compared by
# length and digits, not floating point or bounded shell integers.
gh api --paginate --slurp "repos/$GH_REPO/releases?per_page=100" > "$RUNNER_TEMP/patched-releases.json"
make_latest=$(python3 - "$RUNNER_TEMP/patched-releases.json" <<'PY'
import json, os, re, sys
pattern = re.compile(r'patched-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', re.ASCII)
def key(tag):
    match = pattern.fullmatch(tag)
    return tuple((len(part), part) for part in match.groups()) if match else None
candidate = key(os.environ['RELEASE_TAG'])
assert candidate is not None
public = [key(r['tag_name']) for page in json.load(open(sys.argv[1])) for r in page
          if not r['draft'] and not r['prerelease']]
print('false' if any(k is not None and k >= candidate for k in public) else 'true')
PY
)
# Last Git operation before making the assembled draft public: re-fetch the tag
# and peel it to the exact tested commit. Tag rulesets must prevent concurrent moves.
revalidate_candidate "$CANDIDATE_SHA" "$RELEASE_TAG"
gh api --method PATCH "repos/$GH_REPO/releases/$release_id" -F draft=false -f make_latest="$make_latest" >/dev/null
