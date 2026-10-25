package main

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/depcheck"
)

// The agent runs on every host; the server's code and libraries stay out.
func TestNoServerDependencies(t *testing.T) {
	depcheck.Forbid(t, depcheck.Server...)
}
