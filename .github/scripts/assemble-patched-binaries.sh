#!/usr/bin/env bash
# Bash | Go toolchain | Four-platform patched binary assembly
# Check: bash -n .github/scripts/assemble-patched-binaries.sh
# Run: RELEASE_TAG=... CANDIDATE_SHA=... UPSTREAM_BASE=... RUNNER_TEMP=... bash .github/scripts/assemble-patched-binaries.sh
# Deps: git, go, python3, tar, sha256sum. No network or application execution.
set -euo pipefail
source .github/scripts/patched-release-common.sh
: "${RELEASE_TAG:?Exact patched release tag required}"
: "${CANDIDATE_SHA:?Exact reviewed commit required}"
: "${UPSTREAM_BASE:?Upstream provenance required}"
: "${RUNNER_TEMP:?External runner temporary directory required}"
canonical_patched_tag "$RELEASE_TAG"
[[ "$CANDIDATE_SHA" =~ ^[0-9a-f]{40}$ ]]
[[ "$UPSTREAM_BASE" == unknown || "$UPSTREAM_BASE" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ $(git rev-parse 'HEAD^{commit}') == "$CANDIDATE_SHA" ]]
repo=$(git rev-parse --show-toplevel)
[[ $(pwd -P) == "$repo" ]]
# Output must not pre-exist: never overwrite files left by another build.
[[ ! -e dist && ! -L dist ]]
python3 - "$repo" "$RUNNER_TEMP" <<'PY'
from pathlib import Path
import sys
root, temporary = (Path(value).resolve() for value in sys.argv[1:])
if temporary == root or root in temporary.parents or not temporary.is_dir():
    raise SystemExit('RUNNER_TEMP must be an existing directory outside the checkout')
PY
work=$(mktemp -d "$RUNNER_TEMP/patched-binaries.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/archives"
for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
  # Inspect untracked files too; do not hide dirty sources with -buildvcs=false.
  [[ -z $(git status --porcelain --untracked-files=normal) ]]
  goos=${target%_*}
  goarch=${target#*_}
  workdir="$work/$target"
  mkdir -p "$workdir"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags="-s -w -X github.com/codex2api/internal/version.Version=$RELEASE_TAG -X github.com/codex2api/internal/version.Source=patched -X github.com/codex2api/internal/version.UpstreamBase=$UPSTREAM_BASE -X github.com/codex2api/internal/version.Revision=$CANDIDATE_SHA" \
    -o "$workdir/codex2api" .
  go version -m "$workdir/codex2api" > "$work/build-info.txt"
  python3 - "$work/build-info.txt" "$CANDIDATE_SHA" "$goos" "$goarch" <<'PY'
from pathlib import Path
import sys
settings = {}
for line in Path(sys.argv[1]).read_text().splitlines():
    fields = line.strip().split('\t', 1)
    if len(fields) == 2 and fields[0] == 'build' and '=' in fields[1]:
        key, value = fields[1].split('=', 1)
        settings[key] = value
expected = {'vcs': 'git', 'vcs.revision': sys.argv[2], 'vcs.modified': 'false',
            'GOOS': sys.argv[3], 'GOARCH': sys.argv[4], 'CGO_ENABLED': '0'}
if any(settings.get(key) != value for key, value in expected.items()):
    raise SystemExit('Binary build provenance/platform verification failed')
PY
  cp .env.example README.md "$workdir/"
  tar -C "$workdir" -czf "$work/archives/codex2api_${RELEASE_TAG}_${target}.tar.gz" codex2api .env.example README.md
  printf 'Verified clean binary: %s\n' "$target"
done
[[ -z $(git status --porcelain --untracked-files=normal) ]]
(cd "$work/archives" && sha256sum codex2api_*.tar.gz > SHA256SUMS.txt)
# Only expose upload assets after every Go build and its provenance check passed.
mkdir dist
cp "$work/archives/"* dist/
