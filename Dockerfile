# syntax=docker/dockerfile:1.8
# check=error=true

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ENV CGO_ENABLED=0 \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/root/.cache/go-build \
    GOTOOLCHAIN=local \
    TZ=UTC \
    SOURCE_DATE_EPOCH=0

WORKDIR /workspace

# warm up module cache
COPY go.mod go.sum ./
RUN \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# copy sources
COPY . .


# target parameters for cross-compilation
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
ARG REVISION

# build the binary
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS:-$(go env GOOS)} \
    GOARCH=${TARGETARCH:-$(go env GOARCH)} \
    go build \
      -v \
      -o /workspace/argo-assistant \
      -trimpath \
      -mod=readonly \
      -buildvcs=false \
      -tags netgo,osusergo,timetzdata \
      -pgo=auto \
      -ldflags "-s -w -buildid= \
                -extldflags '-static' \
                -X 'main.version=${VERSION}' \
                -X 'main.revision=${REVISION}'" \
      .

# minimal runtime image
FROM scratch

# copy CA certificates for HTTPS support
# (taken from the same alpine version)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# copy the binary (read/execute permissions are enough)
COPY --from=build --chmod=0555 /workspace/argo-assistant /usr/local/bin/argo-assistant

# run as non-root (65532 = nobody in most base images)
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/argo-assistant"]