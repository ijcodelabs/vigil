# Go applications

How to get a Go service into [Services and traces](apm.md): a span for
every request and outgoing call, its log lines linked to their trace.
Go has no agent that instruments a binary from outside; the service sets
up the OpenTelemetry SDK itself, in about thirty lines. vigil's own server
does it this way (`internal/cli/servercli/tracing.go`).

## 1. The agent listens for OTLP

As in [Services and traces](apm.md#switch-it-on):

```yaml
config:
  otlp:
    enabled: true
```

## 2. Set up the SDK

```bash
go get go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk \
  go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp \
  go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
```

```go
// tracing.go
func setupTracing(ctx context.Context) (shutdown func(context.Context) error, err error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil // off unless configured
	}
	exp, err := otlptracehttp.New(ctx) // reads OTEL_EXPORTER_OTLP_*
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.Default()), // reads OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}
```

Call it first thing in `main`, and the shutdown on the way out so the last
spans are sent:

```go
shutdown, err := setupTracing(ctx)
if err != nil {
	log.Fatal(err)
}
defer shutdown(context.Background())
```

## 3. Instrument the server and the clients

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /orders/{id}", getOrder)
srv := &http.Server{Handler: otelhttp.NewHandler(mux, "http")}

client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
```

- With Go's `ServeMux`, the span is named after the pattern that matched
  (`GET /orders/{id}`), not the path, so the operations stay a short list.
- Outgoing requests through that client carry the trace context, so the
  next service continues the same trace. Every service the request
  passes needs both: a client that sends the context and a server that
  reads it.
- For `database/sql`, `github.com/XSAM/otelsql` wraps the driver; for
  gRPC, `otelgrpc`. Pass the request's `ctx` down to every call, or the
  spans lose their parent.

## 4. Link log lines to traces

Write logs as JSON with the trace id of the request, and let the agent
parse them:

```go
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

logger := slog.New(traceHandler{slog.NewJSONHandler(os.Stdout, nil)})
logger.InfoContext(ctx, "order stored", "order", id) // the Context variants carry the trace
```

On the agent, the log source of the service parses JSON
([agent configuration](configs.md), `parse: json`). A line's `trace_id`
then opens its trace in Explore, and a trace has a **Logs of this trace**
button. Logs written without a context (`logger.Info`) have no trace id.

## 5. Start it

```bash
export OTEL_SERVICE_NAME=orders
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod,service.namespace=shop,service.version=1.4.2
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
./orders
```

`otlptracehttp` speaks OTLP over HTTP with protobuf, which is what the
agent takes. In a container, use the agent's address as the containers
see it; with OTLP tokens, add
`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer%20<token>"`.

## What to keep out of spans

`otelhttp` records the method, the route, the status, and also the
client's address (`client.address`) and user agent. Attributes you add
with `span.SetAttributes` are yours to keep free of personal data, tokens
and message text. vigil's server writes its own server spans without
`otelhttp` for that reason: they carry only the method, the route and the
status.

## Check it

1. Send a few requests.
2. **Services** lists the service after about a minute; its page shows
   the operations by route and what it calls.
3. A request that went through two services is one trace with both in it.
