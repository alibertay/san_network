# syntax=docker/dockerfile:1
#
# Go SAN node image with the real LMDB backend (cgo). Build from the repo root:
#
#   docker build -f deploy/Dockerfile -t san-network-go .
#
# The lmdb-go driver compiles its bundled LMDB (mdb.c), so only build-essential
# (gcc/make) is required; liblmdb-dev is not needed. The runtime is distroless
# Debian 12 (glibc + CA certificates) running as the nonroot user.

FROM golang:1.26 AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=1 go build -tags lmdb -trimpath \
        -ldflags="-s -w -X github.com/alibertay/san_network/internal/version.Version=${VERSION} \
                  -X github.com/alibertay/san_network/internal/version.Commit=${COMMIT} \
                  -X github.com/alibertay/san_network/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/sannode ./cmd/sannode \
    && CGO_ENABLED=1 go build -tags lmdb -trimpath \
        -ldflags="-s -w -X github.com/alibertay/san_network/internal/version.Version=${VERSION} \
                  -X github.com/alibertay/san_network/internal/version.Commit=${COMMIT} \
                  -X github.com/alibertay/san_network/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/sanup ./cmd/sanup \
    && CGO_ENABLED=1 go build -tags lmdb -trimpath \
        -ldflags="-s -w -X github.com/alibertay/san_network/internal/version.Version=${VERSION} \
                  -X github.com/alibertay/san_network/internal/version.Commit=${COMMIT} \
                  -X github.com/alibertay/san_network/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/sancli ./cmd/sancli \
    && mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/base-debian12:nonroot

LABEL org.opencontainers.image.title="san-node" \
      org.opencontainers.image.description="SAN Network node (LMDB backend)" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /out/sannode /usr/local/bin/sannode
COPY --from=builder /out/sanup /usr/local/bin/sanup
COPY --from=builder /out/sancli /usr/local/bin/sancli
COPY --from=builder --chown=65532:65532 /out/data /var/lib/san

ENV SAN_DATA_DIR=/var/lib/san \
    SAN_KEY_FILE=/var/lib/san/san_key.json \
    SAN_PEERS_CACHE=/var/lib/san/peers-cache.json \
    SAN_PEER_REGISTRY=/var/lib/san/peers.json \
    SAN_HOST=0.0.0.0 \
    SAN_API_HOST=0.0.0.0 \
    SAN_DB_BACKEND=lmdb \
    SAN_DB_PATH=/var/lib/san/node.db \
    SAN_LOG_FORMAT=json

# REST API and the three gRPC P2P ports.
EXPOSE 8000 8765 8769 8770
VOLUME ["/var/lib/san"]

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/sancli", "--rpc", "http://127.0.0.1:8000", "health"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/sanup", "--foreground"]
