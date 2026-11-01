package servercli

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/ijcodelabs/vigil/internal/version"
)

// setupTracing sends the server's own spans by OTLP over HTTP when the
// standard variables name an endpoint (OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT; a vigil agent's OTLP endpoint is
// the usual one). Without them nothing is set up and the server's spans
// are no-ops. OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES and
// OTEL_TRACES_SAMPLER work as the SDK documents them.
func setupTracing(ctx context.Context, log *slog.Logger) func() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func() {}
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Warn("tracing is off: the exporter could not be set up", "err", err)
		return func() {}
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("vigil-server"), semconv.ServiceVersion(version.Version)))
	if err != nil {
		res = resource.Default()
	}
	// resource.Default already read OTEL_SERVICE_NAME; ours would win, so
	// put it back when one was given.
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		res, _ = resource.Merge(res, resource.NewSchemaless(semconv.ServiceName(name)))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	log.Info("tracing on: the server's spans go out by OTLP")
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tp.Shutdown(ctx)
	}
}
