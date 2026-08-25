#!/usr/bin/env bash
# Build and publish this fork to npm under the @alessandrolattao scope.
#
# Upstream's release path (goreleaser -> sdk/js/scripts/release.ts) also cuts
# Rust, Python and Docker artifacts for every platform SST supports. This fork
# only needs the CLI, on the platforms we actually run it on, so it builds the
# binaries directly and assembles the same three-package layout by hand:
#
#   @alessandrolattao/sst               wrapper + JS SDK, resolves the binary
#   @alessandrolattao/sst-linux-x64     the binary for CI checks and laptops
#   @alessandrolattao/sst-linux-arm64   the binary for the arm64 deploy runner
#
# The wrapper declares the two as optionalDependencies, and npm installs
# whichever matches the host's os/cpu.
#
# Usage:
#   scripts/fork-publish.sh                 # bump the pre-release counter
#   scripts/fork-publish.sh 4.17.1-mine.7   # publish exactly this version
#   DRY_RUN=1 scripts/fork-publish.sh       # build and pack, publish nothing
set -euo pipefail

SCOPE="@alessandrolattao"
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# Both the version and the `commit` field below describe HEAD, so a dirty tree
# would publish an artifact built from something no commit contains while
# claiming otherwise. CI is always clean; this is for the local path.
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "fork-publish: working tree is dirty, commit or stash before publishing" >&2
  git status --short >&2
  exit 1
fi

# Resolve the version to publish. Without an argument, take the highest one
# already on npm and bump its trailing counter, so the pipeline is idempotent
# in the sense that it never tries to republish a taken version (npm refuses
# those, and rightly so: published versions are immutable).
version="${1:-}"
if [ -n "$version" ]; then
  # `go build -ldflags "... -X main.version=$version"` splits its value on
  # spaces, so an unvalidated version is a way to inject linker flags into a
  # binary that then gets published. Semver has no spaces; require it.
  if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "fork-publish: '$version' is not a semver version" >&2
    exit 1
  fi
fi
if [ -z "$version" ]; then
  # The base is whatever upstream release this fork currently sits on, so the
  # published version says what it is built from. The counter distinguishes
  # our own iterations on top of that same upstream release, and restarts
  # whenever we rebase onto a newer one.
  base=$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')
  if [ -z "$base" ]; then
    echo "fork-publish: no upstream tag reachable from HEAD; pass the version explicitly" >&2
    exit 1
  fi

  # The counter is how many commits this fork carries on top of that tag, so
  # the version is a function of the tree alone. Deriving it from the registry
  # instead looked simpler and was not: `npm view` returns the `latest`
  # dist-tag rather than the highest version, a network blip reads as "never
  # published" and resets the counter onto a version that already exists, and
  # two runs of the same commit produce two different versions. It also wedged
  # permanently if the wrapper failed after the platform packages published,
  # since immutability is per package but the counter was read from one of them.
  counter=$(git rev-list --count "$(git describe --tags --abbrev=0)..HEAD")

  version="${base}-${SUFFIX_NAME:-alessandrolattao}.${counter}"
  echo "fork-publish: upstream $base, $counter commits on top, publishing $version"
fi

# The platform assets (bridge binary, runtime shims, dockerfiles) are embedded
# into the CLI with //go:embed, so they must exist before `go build` or the
# binary ships whatever was last left in platform/dist.
#
# Run under `bash -e`: that script has no `set -e` of its own and ends on a
# `cd`, so its exit status is success no matter what failed inside it. Without
# this a half-built platform ships silently, which is the exact failure this
# step exists to prevent. It also drives a multi-arch buildx build, so QEMU and
# buildx have to be set up by the caller.
echo "fork-publish: building platform assets"
bash -e platform/scripts/build

# --frozen-lockfile with no fallback: resolving fresh dependencies here would
# put whatever the registry served at that moment into the published wrapper.
# dist/ is cleared first: `tsc` overwrites what it emits but never removes the
# output of sources that no longer exist, and that leftover would be copied
# into the package.
echo "fork-publish: building the JS SDK"
(cd sdk/js && rm -rf dist && bun install --frozen-lockfile && bun run build)

staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

# What every published package points back to. npm only fills in gitHead when
# publishing from inside a git repo, and this publishes from a staging dir.
commit=$(git rev-parse HEAD)

# One package per platform: the binary plus a package.json whose os/cpu fields
# are what let npm pick the right one and skip the others.
declare -a platform_packages=()
for target in "linux amd64 x64" "linux arm64 arm64"; do
  read -r goos goarch cpu <<<"$target"
  name="sst-${goos}-${cpu}"
  dir="$staging/$name"
  mkdir -p "$dir/bin"

  echo "fork-publish: building $goos/$goarch"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -ldflags "-s -w -X main.version=$version" -o "$dir/bin/sst" ./cmd/sst

  jq -n \
    --arg name "$SCOPE/$name" --arg version "$version" \
    --arg os "$goos" --arg cpu "$cpu" --arg commit "$commit" \
    '{name: $name, version: $version, license: "MIT", commit: $commit,
      repository: {type: "git", url: "git+https://github.com/alessandrolattao/sst.git"},
      os: [$os], cpu: [$cpu]}' > "$dir/package.json"
  # Declaring MIT without shipping its text is a claim with nothing behind it.
  cp LICENSE "$dir/LICENSE"

  platform_packages+=("$dir")
done

# The wrapper carries the JS SDK and the launcher, and points at the platform
# packages by exact version so a half-published release cannot mix versions.
wrapper="$staging/sst"
mkdir -p "$wrapper"
cp -r sdk/js/bin sdk/js/dist "$wrapper/"
cp README.md "$wrapper/README.md"
cp LICENSE "$wrapper/LICENSE"
# `commit` is what ties a published version back to a tree. Nothing else does:
# npm only fills in gitHead when publishing from inside a git repo, and this
# publishes from a staging directory. CI reads it back to tell whether the
# current branch head has already been released.
jq \
  --arg version "$version" --arg scope "$SCOPE" --arg commit "$commit" \
  '.version = $version
   | .commit = $commit
   | .optionalDependencies = {
       ($scope + "/sst-linux-x64"): $version,
       ($scope + "/sst-linux-arm64"): $version,
     }' sdk/js/package.json > "$wrapper/package.json"

if [ "${DRY_RUN:-}" = "1" ]; then
  echo "fork-publish: DRY_RUN, packing instead of publishing"
  for dir in "${platform_packages[@]}" "$wrapper"; do
    (cd "$dir" && npm pack --dry-run 2>&1 | tail -3)
  done
  exit 0
fi

# Platforms first: the wrapper depends on them, so publishing it first would
# leave a window where installing it resolves nothing.
for dir in "${platform_packages[@]}" "$wrapper"; do
  echo "fork-publish: publishing $(jq -r .name "$dir/package.json")"
  (cd "$dir" && npm publish --access public)
done

echo "fork-publish: published $version"
echo "version=$version" >> "${GITHUB_OUTPUT:-/dev/null}"
