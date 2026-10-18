# Registry garbage-collection sidecar: Distribution plus the registry-gc binary.
FROM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/registry-gc/ cmd/registry-gc/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/registry-gc ./cmd/registry-gc

FROM registry:3.0.0
COPY --from=builder /out/registry-gc /usr/local/bin/registry-gc
USER 65532
ENTRYPOINT ["/usr/local/bin/registry-gc"]
