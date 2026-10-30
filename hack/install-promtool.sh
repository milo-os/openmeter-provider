#!/usr/bin/env bash
set -euo pipefail

readonly VERSION="3.5.0"
readonly RELEASES="https://github.com/prometheus/prometheus/releases/download"

die() {
  echo "❌ $*" >&2
  exit 1
}

usage() {
  {
    echo "usage: bin/install-promtool <install-directory>"
    echo "       bin/install-promtool --version"
    echo
    echo "Installs promtool ${VERSION} into install-directory, checked against the SHA-256"
    echo "pinned here, and puts that directory first on PATH for later workflow steps."
  } >&2
  exit 2
}

expected_sha256() {
  case "$1" in
    linux-amd64) echo "e811827af26d822afb09a4f28314f61b618b12cff5369835a67f674d8b46f39a" ;;
    linux-arm64) echo "173389cc42bf09c4e6e54cb53fa07a5a835d7c261e14775d2183181d6e385d1c" ;;
    darwin-amd64) echo "5856d0c306e7d852d9a79788b20acb630501b3fab120b69f5317d6b0732dea57" ;;
    darwin-arm64) echo "c17bc2c992f0b5515eebe2b4ccd626214f8e309a0fe6ec40ed13c42ec13d05fb" ;;
    *) die "no SHA-256 is pinned for Prometheus ${VERSION} on $1, so a download cannot be verified" ;;
  esac
}

platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported operating system $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  printf '%s-%s\n' "$os" "$arch"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    die "neither sha256sum nor shasum is available, so the download cannot be verified"
  fi
}

case "${1:-}" in
  --version) printf '%s\n' "$VERSION"; exit 0 ;;
  -*|"") usage ;;
esac
[[ $# -eq 1 ]] || usage

command -v curl >/dev/null 2>&1 || die "curl is required to download promtool"

target="$(platform)"
want="$(expected_sha256 "$target")"
name="prometheus-${VERSION}.${target}"

mkdir -p "$1" || die "cannot create $1"
install_dir="$(cd "$1" && pwd)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "${work}/${name}.tar.gz" "${RELEASES}/v${VERSION}/${name}.tar.gz" \
  || die "could not download ${RELEASES}/v${VERSION}/${name}.tar.gz"

got="$(sha256_of "${work}/${name}.tar.gz")"
[[ "$got" == "$want" ]] \
  || die "${name}.tar.gz hashes to ${got}, not the ${want} pinned for ${VERSION}"

tar xzf "${work}/${name}.tar.gz" -C "$work" "${name}/promtool" \
  || die "${name}.tar.gz does not contain promtool"

install -m 0755 "${work}/${name}/promtool" "${install_dir}/promtool" \
  || die "could not install promtool into ${install_dir}"

found="$("${install_dir}/promtool" --version 2>&1 | awk 'NR == 1 { print $3 }')"
[[ "$found" == "$VERSION" ]] || die "the download reports promtool ${found:-unknown}, not ${VERSION}"

if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "$install_dir" >> "$GITHUB_PATH"
fi

echo "promtool ${VERSION} installed: ${install_dir}/promtool"
