FROM golang:1.24-alpine AS build

WORKDIR /workspace
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o argo-assistant -ldflags '-w -extldflags "-static"' .

FROM scratch

COPY --from=build --chmod=777 /workspace/argo-assistant /usr/local/bin/argo-assistant