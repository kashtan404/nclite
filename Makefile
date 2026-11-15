# Default build: pure Go, no cgo, runs on any aarch64 Linux (Entware glibc or musl alike).
build-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/nclite ./cmd/nclite

# Fallback if modernc sqlite misbehaves on the router kernel: build natively with Entware gcc.
build-cgo:
	CGO_ENABLED=1 go build -tags cgo_sqlite -trimpath -ldflags="-s -w" -o dist/nclite ./cmd/nclite

test:
	CGO_ENABLED=1 go test -tags cgo_sqlite -race ./...
