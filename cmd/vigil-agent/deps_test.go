package main

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/depcheck"
)

// The agent runs on every host; the server's code and libraries stay out,
// and so does gRPC: OTLP comes in over HTTP, and the generated OTLP service
// packages that would bring it are replaced by internal/otlpwire.
func TestNoServerDependencies(t *testing.T) {
	depcheck.Forbid(t, append(depcheck.Server,
		"google.golang.org/grpc",
		"github.com/grpc-ecosystem/",
		"go.opentelemetry.io/proto/otlp/collector/")...)
}
