#!/bin/bash
# build.sh IMAGE — builds OpenMeter, at the version the kind cluster's chart
# runs, with patch.py's clock, as IMAGE. Upstream's own Dockerfile does the
# build. Only for test/timetravel: the image is loaded into kind and never
# pushed.
set -euo pipefail

image=$1
dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$dir/../../.." && pwd)
# The chart version is OpenMeter's, without the leading v.
version="v$(sed -nE 's/^ *version: "([^"]+)".*/\1/p' "$root/config/dependencies/openmeter/helmrelease.yaml")"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
echo "Building OpenMeter ${version} with the time-travel clock as ${image}..."
git clone --quiet --depth 1 --branch "$version" https://github.com/openmeterio/openmeter "$work/src"
python3 "$dir/patch.py" "$work/src"
# Only the binaries that run billing are rebuilt (the API server, the
# billing worker and the jobs), onto upstream's image: the others don't
# bill, so they keep the real time. Built for the machine's own platform,
# which kind's node runs on, two packages at a time to fit in Docker
# Desktop's memory beside a kind cluster.
go_version=$(sed -nE 's/^go ([0-9.]+).*/\1/p' "$work/src/go.mod")
cat >"$work/Dockerfile" <<DOCKERFILE
FROM golang:${go_version}-alpine AS builder
RUN apk add --no-cache musl-dev gcc git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1 GOFLAGS=-p=2
RUN for cmd in server:openmeter billing-worker:openmeter-billing-worker jobs:openmeter-jobs; do \\
      go build -ldflags "-linkmode external -extldflags -static -X main.version=${version}-timetravel" \\
        -tags musl -o "/out/\${cmd#*:}" "./cmd/\${cmd%%:*}" || exit 1; \\
    done
FROM ghcr.io/openmeterio/openmeter:${version}
COPY --from=builder /out/ /usr/local/bin/
DOCKERFILE
docker build -f "$work/Dockerfile" -t "$image" "$work/src"
