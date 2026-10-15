#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: scripts/build-release.sh VERSION (for example, v1.0.0)" >&2
  exit 2
fi

release_version=$1
version_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if [[ ! $release_version =~ $version_pattern ]]; then
  echo "Invalid version: expected vMAJOR.MINOR.PATCH with an optional prerelease suffix." >&2
  exit 2
fi
if [[ $release_version == *-* ]]; then
  IFS='.' read -r -a prerelease_parts <<< "${release_version#*-}"
  for part in "${prerelease_parts[@]}"; do
    if [[ $part =~ ^0[0-9]+$ ]]; then
      echo "Invalid version: numeric prerelease identifiers must not have leading zeros." >&2
      exit 2
    fi
  done
fi

project_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$project_root"
mkdir -p dist
staging_dir=$(mktemp -d "$project_root/dist/.build-XXXXXX")
trap 'rm -rf -- "$staging_dir"' EXIT

host_os=$(go env GOHOSTOS)
host_arch=$(go env GOHOSTARCH)
assets=()

for target_os in darwin linux; do
  for target_arch in arm64 amd64; do
    asset="cida-${target_os}_${target_arch}-${release_version}"
    echo "Building $asset"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
      go build -trimpath -ldflags="-s -w -X main.version=$release_version" \
      -o "$staging_dir/$asset" .
    assets+=("$asset")

    if [[ $target_os == "$host_os" && $target_arch == "$host_arch" ]]; then
      actual_version=$("$staging_dir/$asset" --version)
      if [[ $actual_version != "cida $release_version" ]]; then
        echo "Version check failed: $actual_version" >&2
        exit 1
      fi
    fi
  done
done

cp licenses/Cida.txt "$staging_dir/LICENSE-Cida.txt"
assets+=(LICENSE-Cida.txt)
(
  cd "$staging_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${assets[@]}" > SHA256SUMS
  else
    shasum -a 256 "${assets[@]}" > SHA256SUMS
  fi
)

# Expose the output only after all four builds and the native version check pass.
output_dir="$project_root/dist/$release_version"
mkdir -p "$output_dir"
for asset in "${assets[@]}" SHA256SUMS; do
  mv -f -- "$staging_dir/$asset" "$output_dir/$asset"
done
echo "Release files are ready in $output_dir"
