# Build any last-operator binary: docker build --build-arg CMD=gateway .
FROM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CMD=operator
ARG VERSION=dev
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X github.com/moogie-labs/last-operator/internal/gateway.Version=${VERSION}" \
    -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
