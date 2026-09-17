//go:build ignore

// Dockerfile.go prints the multi-stage Dockerfile for the Go node image.
//
// The Go toolchain parses every non-dot *.go file in the module, so a file
// named Dockerfile.go cannot hold raw Dockerfile syntax without breaking
// `gofmt -l .`, `go build ./...`, `go vet ./...` and `go test ./...`.
// Keeping the definition executable satisfies both toolchains:
//
//	go run Dockerfile.go | docker build -f- -t san-network-go .
//
// The Python reference image lives in the unchanged `Dockerfile`. This image
// disables cgo, so it runs with the in-memory store (SAN_DB_BACKEND=memory);
// for LMDB persistence build with CGO_ENABLED=1 -tags lmdb on a glibc runtime
// such as gcr.io/distroless/base-debian12 (the bundled LMDB needs no liblmdb).
package main

import (
	"fmt"
	"os"
)

const dockerfile = `# syntax=docker/dockerfile:1

# Go node image (the Python Dockerfile remains the reference deployment).
FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sannode ./cmd/sannode \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sancli ./cmd/sancli

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/sannode /usr/local/bin/sannode
COPY --from=builder /out/sancli /usr/local/bin/sancli

ENV SAN_HOST=0.0.0.0 \
    SAN_DB_BACKEND=memory

# REST API and the three gRPC P2P ports.
EXPOSE 8000 8765 8769 8770

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/sancli", "--rpc", "http://127.0.0.1:8000", "health"]

ENTRYPOINT ["/usr/local/bin/sannode"]
CMD ["serve"]
`

func main() {
	if _, err := os.Stdout.WriteString(dockerfile); err != nil {
		fmt.Fprintln(os.Stderr, "Dockerfile.go:", err)
		os.Exit(1)
	}
}
