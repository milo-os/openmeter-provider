# Build the openmeter-provider binary
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
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
    -o openmeter-provider ./cmd/openmeter && \
  CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.gitCommit=${GIT_COMMIT} \
      -X main.gitTreeState=${GIT_TREE_STATE} \
      -X main.buildDate=${BUILD_DATE}" \
    -o submission-consumer ./cmd/openmeter/submission-consumer

# Use distroless as minimal base image to package the binaries
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/openmeter-provider .
COPY --from=builder /workspace/submission-consumer .
USER 65532:65532

ENTRYPOINT ["/openmeter-provider"]
