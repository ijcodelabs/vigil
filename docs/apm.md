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

Step by step, with log lines linked to traces: [Python](apm-python.md),
[Go](apm-go.md).

In a container, the agent's endpoint has to be reachable from it: let the
agent listen on an address the containers can reach (`listen: 0.0.0.0:4318`
on a host you firewall, or the Docker bridge address) and use that instead
of `localhost`. On Kubernetes, the node's address
(`http://$(HOST_IP):4318` with `HOST_IP` from `status.hostIP`).

### Tokens and TLS

On loopback the endpoint takes anything sent to it. Once senders on other
hosts or in containers use it, give each one a token, and serve it over
TLS so the tokens do not travel in clear:

```yaml
config:
  otlp:
    enabled: true
    listen: 0.0.0.0:4318
    tokens_file: /etc/vigil/otlp-tokens   # "name token" per line; # comments
    tls:
      cert_file: /etc/vigil/otlp.crt
      key_file: /etc/vigil/otlp.key
```

With `tokens_file` set, a request without one of its tokens is refused
(401), and everything a sender sends is labelled `emitter=<name>` (a stream
label on logs, a label on metrics), which the sender cannot set itself: an
attribute of its own called `emitter` is kept as `attr_emitter` on logs and
`exported_emitter` on metrics. A name may have two tokens while one is
being replaced. The file and the certificate are read again when they
change, without a restart; a file that cannot be read keeps the tokens from
before, and with none from before nobody gets in. Senders set
`OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer%20<token>"` (the space URL-encoded). Spans are not
labelled with the emitter.

### Client certificates from the server

Instead of tokens, or with them, the endpoint can ask senders for a client
certificate from the server's own authority (the server needs
`tls.self_signed: true`). Certificates live 7 days and are renewed at half
their life; nobody copies a key or a certificate around.

1. An admin approves names once, on the server: which agents may hold
   them, and for the endpoint the names it is reached by.

   ```bash
   curl -X PUT -H "Authorization: Bearer $ADMIN" https://vigil.example.com/api/v1/identities/boop-chat \
     -d '{"kind": "client", "agents": "boop-host"}'
   curl -X PUT -H "Authorization: Bearer $ADMIN" https://vigil.example.com/api/v1/identities/gateway \
     -d '{"kind": "endpoint", "agents": "boop-host", "names": ["192.0.2.10"]}'
   ```

2. The endpoint's agent takes its certificate from the server and checks
   the senders'. When an admin changes the endpoint's names, the agent gets a
   certificate with the new ones within a minute:

   ```yaml
   config:
     otlp:
       enabled: true
       listen: 0.0.0.0:4318
       tls:
         from_server: gateway      # instead of cert_file and key_file
         client_certs: required    # or optional: a certificate or a token
   ```

3. On the sender's host, the agent's **own file** (never a profile) says
   where the sender's files go; the agent writes them, renews them, and
   writes the server's authority for the sender to trust the endpoint:

   ```yaml
   identity:
     clients:
       - name: boop-chat
         cert_file: /srv/boop/otlp/chat.pem
         key_file: /srv/boop/otlp/chat.key
         ca_file: /srv/boop/otlp/ca.pem
         group: 10001              # numeric; the key is then 0640
   ```

   The directory must exist; files are replaced whole and never written
   through a symbolic link. The sender reads them again when they change
   (Boop's emitters do: see Boop).

A certificate's name is the sender's `emitter`, as a token's is. A request
with both must name the same emitter. **Revoking** a name
(`POST /api/v1/identities/{name}/revoke`) moves it to a new epoch: every
certificate ever issued for it is refused at its next request, and none is
issued until the name is approved again (`PUT`), at the new epoch. An
endpoint learns the current epochs from the server every minute; while the
server is unreachable it keeps the last ones it got, so a stolen certificate
works until it expires, at most 7 days. Hosts need correct clocks (NTP).

To see the pages before instrumenting anything:

```bash
vigil tracegen            # demo traces of a small shop, to the local agent
```

Against an agent with `tokens_file`, give it a token with `-token` (or
`VIGIL_OTLP_TOKEN`), or `-token-file` naming a file with the token, or the
agent's tokens file itself (its first token is used).

## What you get

**Services** lists every service with its request rate, share of failed
requests, and median and 95th-percentile duration over the chosen time.
"Requests" are what a service was asked to do: its *server* and *consumer*
spans.

**Map** (the second tab) draws the same services by how deep in the
calls they sit: those nobody else calls on the left, what they call next
to them, databases and outside hosts on the right. A line is calls, thicker
for more, yellow or red when over 1% or 5% of them fail; a box is coloured
by its own share of failed requests. Hover for its numbers, click to open
it. A call between two services shows when the client span names the other
one (`peer.service`), or when stored traces show it.

A **service's page** has the same over time, its operations (each route,
query or message it handles or sends), what it depends on (other services,
databases, queues, outside hosts, taken from its client spans) and who
calls it, and its stored traces: filter to failed ones, slow ones, or one
operation.

**Did a change help?** on a service's page compares the time before a
moment you give (a deploy, a configuration change) with the same time after
it: requests, share of failures, 95th percentile, and a verdict. The same
answer comes from the API (`GET /api/v1/verify`, see [API](api.md)) for a
script or an agent that just made the change.

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
  `slower_than`, and so is a call to a language model that reports its
  tokens (what it cost stays visible); such a span can then appear without
  the rest of its trace, which the trace page says.

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
- Senders authenticate with a bearer token, not a client certificate.
- What a service depends on comes from attributes on its client spans
  (`peer.service`, `db.system`, `messaging.system`, `server.address`). A
  client span that names nothing is not shown as a dependency.
- Traces need VictoriaTraces; there is no fallback to another store.
- Sampling is decided when a span arrives, not after the trace is complete,
  so "store every trace in which anything failed" is not possible; the
  failed span itself is stored.
- The map shows a call between two services only if the client span names
  the other one (`peer.service`) or a stored trace shows it; one known only
  by its address (`server.address`) appears as an outside host.
