package main

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/depcheck"
)

// The relay passes requests on; it needs neither the server's code nor the
// agent's collectors.
func TestNoServerDependencies(t *testing.T) {
	depcheck.Forbid(t, append(depcheck.Server,
		"github.com/ijcodelabs/vigil/internal/agent",
		"github.com/ijcodelabs/vigil/internal/events",
		"github.com/cilium/ebpf")...)
}
