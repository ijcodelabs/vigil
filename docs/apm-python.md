# Python applications

How to get a Python service (FastAPI, Flask, Django, a worker) into
[Services and traces](apm.md): traces of every request and database call,
the service's metrics, and its log lines linked to their trace. It uses
OpenTelemetry's own packages; nothing from vigil goes into the
application.

## 1. The agent listens for OTLP

On the host (or in a profile), as in [Services and traces](apm.md#switch-it-on):

```yaml
config:
  otlp:
    enabled: true
```

## 2. Install the packages

```bash
pip install opentelemetry-distro opentelemetry-exporter-otlp-proto-http
opentelemetry-bootstrap -a install
```

`opentelemetry-bootstrap` looks at what is installed (FastAPI, SQLAlchemy,
httpx, requests, psycopg, redis, Celery, …) and adds the matching
instrumentation packages. Run it again after adding a library, or pin its
output in your requirements file.

## 3. Start the service through `opentelemetry-instrument`

```bash
export OTEL_SERVICE_NAME=checkout
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod,service.namespace=shop,service.version=1.4.2
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_TRACES_EXPORTER=otlp OTEL_METRICS_EXPORTER=otlp OTEL_LOGS_EXPORTER=otlp
export OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED=true
export OTEL_PYTHON_EXCLUDED_URLS="healthz,metrics"

opentelemetry-instrument uvicorn app.main:app --host 0.0.0.0 --port 8000
```

- `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` matters: the agent takes OTLP
  over HTTP with protobuf, not gRPC.
- With `OTEL_LOGS_EXPORTER=otlp` and the logging instrumentation on,
  everything written through `logging` (and libraries such as `structlog`
  that hand records to it) goes to the agent with the trace id of the
  request it belongs to. In Explore a line's `trace_id` then opens its
  trace, and a trace has a **Logs of this trace** button.
- `OTEL_PYTHON_EXCLUDED_URLS` keeps health checks and scrapes out of the
  request figures.
- In a container, use the agent's address as the containers see it, not
  `localhost` ([Services and traces](apm.md#switch-it-on)). Against an
  agent with OTLP tokens, add
  `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer%20<token>"`.

### Servers that fork

The SDK starts a background thread to send batches, and threads do not
survive `fork()`. Two things follow:

- **uvicorn:** run without `--reload` and without `--workers` under
  `opentelemetry-instrument`; run several containers or processes instead
  of workers.
- **gunicorn:** start the SDK in each worker, not in the master. With the
  manual set-up below, call `setup_tracing()` from gunicorn's `post_fork`
  hook.

### Without the wrapper

When a service cannot be started through `opentelemetry-instrument`, set
the SDK up in code and instrument the frameworks explicitly:

```python
from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.sqlalchemy import SQLAlchemyInstrumentor

def setup_tracing(app, engine):
    provider = TracerProvider(resource=Resource.create())  # reads OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))  # reads OTEL_EXPORTER_OTLP_*
    trace.set_tracer_provider(provider)
    FastAPIInstrumentor.instrument_app(app, excluded_urls="healthz,metrics")
    SQLAlchemyInstrumentor().instrument(engine=engine)
```

## What to keep out of spans

Auto-instrumentation records the route template (`/orders/{order_id}`),
the method, the status and, for database calls, the statement with its
placeholders, not the values. It does not record request or response
headers or bodies unless told to
(`OTEL_INSTRUMENTATION_HTTP_CAPTURE_HEADERS_*`); leave those off. What you
add yourself with `span.set_attribute` is yours to keep free of personal
data, tokens and message text. The agent masks secrets it recognises in
stored spans ([redaction](configs.md)), but it cannot know what a field
means.

The client's address and the user agent of each request are recorded by the
HTTP instrumentation as `client.address` and `user_agent.original`. Behind
a proxy the address is the proxy's.

## Check it

1. Send a few requests to the service.
2. **Services** lists it after about a minute, with requests, errors and
   latency; its page shows operations and what it calls.
3. Open a request under **Traces**; database calls and outgoing HTTP calls
   are spans under it.
4. In **Explore → Logs**, `service:checkout` shows its lines; a line with
   a `trace_id` links to its trace.

If nothing arrives, see [Troubleshooting](troubleshooting.md) and check
that `OTEL_EXPORTER_OTLP_PROTOCOL` is `http/protobuf`.
