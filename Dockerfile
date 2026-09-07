# Build the controller-template binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build
ENV GOCACHE=/root/.cache/go-build
ENV GOTMPDIR=/root/.cache/go-build
RUN --mount=type=cache,target=/go/pkg/mod/ \
  --mount=type=cache,target="/root/.cache/go-build" \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.gitCommit=${GIT_COMMIT} \
      -X main.gitTreeState=${GIT_TREE_STATE} \
      -X main.buildDate=${BUILD_DATE}" \
    -o controller-template ./cmd/controller-template

# Use distroless as minimal base image to package the controller-template binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/controller-template .
USER 65532:65532

ENTRYPOINT ["/controller-template"]
