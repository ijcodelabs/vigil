VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build linux test
build:
	go build -ldflags "$(LDFLAGS)" -o bin/vigil ./cmd/vigil

# Static binaries: copy one file to any Debian/RHEL/Ubuntu host.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vigil-linux-amd64 ./cmd/vigil
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vigil-linux-arm64 ./cmd/vigil

test:
	go test ./...

# Recompile the eBPF probe. Needs a container runtime; set CONTAINER=docker
# if you do not use podman.
CONTAINER ?= podman
.PHONY: bpf
bpf:
	$(CONTAINER) run --rm -v $(CURDIR):/src -w /src docker.io/library/golang:1.26 sh -c \
		'apt-get update -qq && apt-get install -y -qq clang llvm libbpf-dev >/dev/null && go generate ./internal/events/'
