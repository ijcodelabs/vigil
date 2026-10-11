# Services and traces (APM)

Applications instrumented with **OpenTelemetry** send their traces, metrics
and logs to the vigil agent on their host. No collector in between, and no
vendor library: the standard OpenTelemetry SDK or auto-instrumentation for
Java, Python, Go, Node.js, .NET, PHP or Ruby is all an application needs.

vigil then shows, per service: how many requests, how many failed, how long
they took, which operations, what the service depends on, and single
requests as a trace through every service they touched.

## Switch it on

In a profile (or the agent's own file):

```yaml
config:
  otlp:
    enabled: true
    # listen: 127.0.0.1:4318        # OTLP over HTTP, protobuf
    # traces:
    #   sample: 1                   # share of traces stored in full, 0 to 1
    #   slower_than: 1s             # always store spans at least this slow
    #   enabled: true               # false: count nothing, store nothing
```

Point the application at the agent:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_SERVICE_NAME=checkout
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod,service.namespace=shop
```

Examples of starting an application with auto-instrumentation:

```bash
java -javaagent:opentelemetry-javaagent.jar -jar app.jar
opentelemetry-instrument python app.py
node --require @opentelemetry/auto-instrumentations-node/register app.js
```

In a container, the agent's endpoint has to be reachable from it: let the
agent listen on an address the containers can reach (`listen: 0.0.0.0:4318`
on a host you firewall, or the Docker bridge address) and use that instead
of `localhost`. On Kubernetes, the node's address
(`http://$(HOST_IP):4318` with `HOST_IP` from `status.hostIP`).

To see the pages before instrumenting anything:

```bash
vigil tracegen            # demo traces of a small shop, to the local agent
```

## What you get

**Services** lists every service with its request rate, share of failed
requests, and median and 95th-percentile duration over the chosen time.
"Requests" are what a service was asked to do: its *server* and *consumer*
spans.

A **service's page** has the same over time, its operations (each route,
query or message it handles or sends), what it depends on (other services,
databases, queues, outside hosts, taken from its client spans) and who
calls it, and its stored traces: filter to failed ones, slow ones, or one
operation.

A **trace** is one request as a waterfall: every span as a bar on a common
time axis, nested under the span that started it, coloured by service,
red where it failed. Click a span for what it recorded: attributes, the
error message, an exception's stack trace. "Logs of this trace" shows the
log lines written while handling that request (applications that log
through OpenTelemetry attach the trace id), and a log line with a
`trace_id` links back to its trace from Explore.

## Counting versus storing

Two things happen to every span, and they are separate on purpose:

- **Counted.** The agent counts and times every span into a few metrics.
  The numbers on the Services pages, and alert rules, use these. They are
  exact whatever is stored.
- **Stored.** The span itself is kept so the trace can be looked at. This
  is what takes space, so it can be sampled: `traces.sample: 0.1` stores
  one trace in ten. The choice is made from the trace id, so every service
  keeps or drops the *same* traces and a stored trace is complete. A span
  that **failed** is always stored, and so is one slower than
  `slower_than`; such a span can then appear without the rest of its trace,
  which the trace page says.

Stored spans go to **VictoriaTraces**, a third store next to the ones for
metrics and logs. The server needs to know where it is:

```yaml
storage:
  metrics_url: http://victoriametrics:8428
  logs_url: http://victorialogs:9428
  traces_url: http://victoriatraces:10428
```

`deploy/docker-compose.yml` runs one. Without `traces_url` the Services
pages still show every number (those are metrics), but there are no traces
to open, and the server drops the spans agents send. How long traces are
kept is VictoriaTraces' `-retentionPeriod` (7 days unless set).

Spans share the agent's and the server's byte allowance for logs
(`limits`). A trace is listed a few seconds after it ends.

## Metrics

| Metric | Meaning | Labels |
|---|---|---|
| `traces_spans_total` | spans seen | `service`, `span_name`, `kind` (server, client, producer, consumer), `status` (ok, error) |
| `traces_span_duration_seconds_bucket`, `_sum`, `_count` | how long they took | the same, plus `le` |
| `traces_service_calls_total` | calls a service made to something else | `service`, `peer`, `status` |
| `traces_service_call_duration_seconds_sum`, `_count` | how long those took | `service`, `peer`, `status` |

Internal spans (work inside a service that is neither a request nor a call
out) are stored but not counted. A service with more than 200 different
operation names has the rest counted as `(other)`: operation names should
be routes like `GET /orders/{id}`, not addresses with ids in them.

Rules worth having:

```yaml
groups:
  - name: services
    rules:
      - alert: ServiceErrors
        expr: |
          sum by (service) (rate(traces_spans_total{kind=~"server|consumer",status="error"}[5m]))
            / sum by (service) (rate(traces_spans_total{kind=~"server|consumer"}[5m])) > 0.05
          and sum by (service) (rate(traces_spans_total{kind=~"server|consumer"}[5m])) > 0.2
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: '{{ $value | humanizePercentage }} of requests to {{ $labels.service }} fail'
      - alert: ServiceSlow
        expr: histogram_quantile(0.95, sum by (service, le) (rate(traces_span_duration_seconds_bucket{kind="server"}[5m]))) > 1
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: '95% of requests to {{ $labels.service }} take up to {{ $value | humanizeDuration }}'
```

## Finding traces yourself

The server passes on the trace store's own query APIs to signed-in users.

Spans as rows, filtered with LogsQL (what the trace lists use):

```bash
curl -G -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://vigil.example.com/api/v1/traces/query \
  --data-urlencode 'query="resource_attr:service.name":="checkout" status_code:2 | sort by (_time desc) | limit 20' \
  --data-urlencode 'start=1h'
```

Fields: `name` (the operation), `duration` (nanoseconds), `status_code` (2
is failed), `status_message`, `kind` (2 server, 3 client, 4 producer, 5
consumer, 1 internal), `trace_id`, `span_id`, `parent_span_id`, `agent`
(the vigil agent it came through), the span's attributes as
`span_attr:<name>` and the resource's as `resource_attr:<name>`. Names with
a colon or a dot go in quotes, as above.

The Jaeger query API, for a whole trace and for tools that speak it:

```
/api/v1/traces/jaeger/api/traces/<trace id>
/api/v1/traces/jaeger/api/traces?service=checkout&minDuration=1s&start=…&end=…
/api/v1/traces/jaeger/api/services
/api/v1/traces/jaeger/api/services/<service>/operations
/api/v1/traces/jaeger/api/dependencies
```

Grafana's Jaeger data source works against
`https://vigil.example.com/api/v1/traces/jaeger` with the admin token as a
bearer token.

## Limits of this version

- OTLP over HTTP with protobuf only; no gRPC, no JSON.
- What a service depends on comes from attributes on its client spans
  (`peer.service`, `db.system`, `messaging.system`, `server.address`). A
  client span that names nothing is not shown as a dependency.
- Traces need VictoriaTraces; there is no fallback to another store.
- Sampling is decided when a span arrives, not after the trace is complete,
  so "store every trace in which anything failed" is not possible; the
  failed span itself is stored.
- There is no service map drawing yet, only the tables.
