FROM golang:1.24-alpine AS build

WORKDIR /workspace
COPY go.mod .
COPY go.sum .
RUN go mod download

ARG VERSION
ARG REVISION

COPY . .

RUN CGO_ENABLED=0 go build -o argo-assistant \
  -trimpath \
  -mod=readonly \
  -buildvcs=false \
  -tags netgo,osusergo \
  -ldflags "-s -w -buildid= -extldflags '-static' \
            -X 'main.version=${VERSION}' \
            -X 'main.revision=${REVISION}'" \
    .

FROM scratch

COPY --from=build --chmod=777 /workspace/argo-assistant /usr/local/bin/argo-assistant