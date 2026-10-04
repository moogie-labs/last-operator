# Sandbox base image: common CLIs plus the last-operator sandbox runner (spec §11.1).
FROM golang:1.27 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/sandbox-runner/ cmd/sandbox-runner/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/sandbox-runner ./cmd/sandbox-runner

FROM debian:bookworm-slim
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.36.1
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq git python3 \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && curl -fsSLo /tmp/kubectl.sha256 "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" \
 && echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/kubectl \
 && rm /tmp/kubectl.sha256
COPY --from=builder /out/sandbox-runner /last/sandbox-runner
USER 65532:65532
WORKDIR /work
ENV HOME=/work
ENTRYPOINT ["/last/sandbox-runner"]
