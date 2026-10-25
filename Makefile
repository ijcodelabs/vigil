VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ijcodelabs/vigil/internal/version.Version=$(VERSION)

# vigil-agent, vigil-server and vigil-relay each carry only their part.
# vigil holds the operator tools and, for one release, all three as well.
BINS := vigil vigil-agent vigil-server vigil-relay

.PHONY: build linux test size
build:
	for b in $(BINS); do go build -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; done

# Static binaries: copy one file to any Debian/RHEL/Ubuntu host.
linux:
	for arch in amd64 arm64; do for b in $(BINS); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b-linux-$$arch ./cmd/$$b || exit 1; \
	done; done

test:
	go test ./...

# Size budgets in bytes for the static linux/amd64 binaries that run on
# every host. On 2026-10-25 the agent was 21.0 MB and the relay 7.7 MB;
# raise a budget only knowingly, never just to make the check pass.
AGENT_MAX ?= 23000000
RELAY_MAX ?= 9000000
size: linux
	@check() { s=$$(wc -c < bin/$$1-linux-amd64 | tr -d ' '); echo "$$1: $$s bytes (budget $$2)"; [ $$s -le $$2 ] || { echo "$$1 is over its size budget"; exit 1; }; }; \
	check vigil-agent $(AGENT_MAX) && check vigil-relay $(RELAY_MAX)

# Recompile the eBPF probe. Needs a container runtime; set CONTAINER=docker
# if you do not use podman.
CONTAINER ?= podman
.PHONY: bpf
bpf:
	$(CONTAINER) run --rm -v $(CURDIR):/src -w /src docker.io/library/golang:1.26 sh -c \
		'apt-get update -qq && apt-get install -y -qq clang llvm libbpf-dev >/dev/null && go generate ./internal/events/'
