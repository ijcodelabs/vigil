# Agent configuration

An agent's configuration has two parts:

- **who it is and where the server is**: always in its own file;
- **what it collects**: either in the same file, or, better with more than
  a few hosts, in profiles on the server ([Fleet](fleet.md)). The format is
  the same in both places. When a profile matches an agent, the collection
  settings in its own file are replaced by what the profiles say.

`configs/agent.example.yaml` in the repository shows every option.

## The part that is always local

```yaml
server:
  url: https://vigil.example.com
  token: ${VIGIL_TOKEN}          # ${VAR} is read from the environment (local file only)
agent_id: web-01                 # default: the host name
labels:                          # what this host is; profiles select by these
  env: prod
  role: web
spool_dir: /var/lib/vigil/spool  # unsent data waits here
spool_max_bytes: 536870912       # beyond this the oldest is dropped
remote_config: true              # false: never take config from the server
```

Labels are added to every metric and log line the agent sends, so
`{env="prod"}` works in any query.

## How often

```yaml
interval: 15s        # for everything that does not set its own
host_metrics: true   # CPU, memory, disks, network, pressure of this host
```

## Recipes

### Scrape a Prometheus endpoint

```yaml
scrape:
  static:
    - job: node
      targets: ["localhost:9100"]
    - job: app
      targets: ["localhost:8443", "localhost:8444"]
      scheme: https
      path: /internal/metrics
      interval: 60s                  # this job only
      timeout: 5s
      bearer_token: ${APP_TOKEN}     # or basic_auth: {username: u, password: p}
      tls_insecure_skip_verify: true
      drop: ['^go_', '^process_']    # regular expressions on metric names
      labels: {team: payments}
```

`keep` is the opposite of `drop`: with it, only matching metrics are sent.
Every job also reports `up` (1 or 0) and `scrape_duration_seconds`.

### Ship a log file

```yaml
logs:
  files:
    - name: app                       # lets a profile change this source later
      paths: [/var/log/app/*.log]
      exclude: ['debug-*']
      labels: {app: shop}
```

A file that already exists when the agent first sees it is read from its
end. Add `from_beginning: true` to ship what is already in it. Rotation by
renaming and by truncating are both followed.

### Parse the lines: templates

A template says how one kind of log is read. A source names its template.

```yaml
logs:
  templates:
    nginx_access:
      parse: regex                    # named groups become fields
      pattern: '^(?P<client>\S+) \S+ \S+ \[(?P<time>[^\]]+)\] "(?P<method>\S+) (?P<path>\S+) [^"]*" (?P<status>\d+) (?P<bytes>\d+)'
      timestamp: {field: time, layout: '02/Jan/2006:15:04:05 -0700'}
      labels: [status]                # make this field a searchable label
      drop: ['GET /healthz']          # discard matching lines
      add: {app: nginx}               # a fixed label on every line of this kind
    app_json:
      parse: json                     # nested keys become a.b.c
      message_field: msg
      timestamp: {field: ts, layout: rfc3339}     # or unix, unix_ms, or a Go layout
      labels: [level]
      mask:                           # hide secrets before anything else sees them
        - {pattern: '"password":"[^"]*"', replace: '"password":"***"'}
    java:
      multiline: {start: '^\d{4}-\d{2}-\d{2}'}    # other lines join the entry above: stack traces
    keyvalue:
      parse: logfmt                   # key=value key="quoted value"
      keep: ['level=(warn|error)']    # ship only matching lines
  files:
    - {name: nginx, paths: [/var/log/nginx/access.log], template: nginx_access}
```

Keep `labels` to fields with few different values (status, level). A label
with thousands of values (a user id, a URL) slows searching down; leave such
fields as ordinary fields, which are still searchable.

### System logs

```yaml
logs:
  journald:
    enabled: true
    units: [nginx.service, sshd.service]   # leave out for everything
    template: plain                        # for every unit without one of its own
    unit_templates:                        # by unit name or syslog identifier; * and ? allowed
      "shop*.service": java                # e.g. join stack traces
      kernel: kernel_kv
```

Every line carries where it came from:

| | |
|---|---|
| `unit` (label) | the systemd unit; a user's unit for user services; `kernel` for what the kernel says (firewall blocks, hardware, the OOM killer); `audit` for audit records. A few lines have none: short-lived programs that log through syslog outside any unit |
| `identifier` | the program's syslog name (`sshd`, `CRON`, `sudo`) |
| `transport` | how it reached the journal: `journal`, `stdout`, `syslog`, `kernel`, `audit` |
| `level`, `pid` | priority as a word, process id |

**Multi-line entries.** A message that already contains several lines is
one entry, nothing to configure. A program that prints a stack trace line
by line produces one journal entry per line; give its unit a template with
`multiline.start` and they are joined. Joining is done per unit, so other
units logging in between do not break it. An entry is complete when the
next one starts, or when its unit has been quiet for a second.

Messages journald stores as raw bytes (colour codes, another encoding) are
read as text.

Needs `journalctl`, so a host install.

### Containers on a Docker or Podman host

```yaml
docker:
  enabled: true
  socket: /var/run/docker.sock
  template: keyvalue      # default template for container logs
  container_templates:    # other templates, by container or image name
    "haproxy*": haproxy_http
    web: nginx_access
    postgis: postgres
```

This gives the logs of every running container and their CPU, memory and
network use.

Which template a container's output gets, first match wins:

1. the container's own label `vigil.logs.template=<name>`
   (`vigil.logs=false` opts the container out altogether);
2. `container_templates`: each key is compared with the container's name
   and with its image's name without registry and tag (`haproxy` for
   `docker.io/library/haproxy:3.0`), and may use `*` and `?`. An exact name
   beats a pattern, a longer pattern a shorter one. This is for containers
   you cannot or do not want to label;
3. `template`, the default.

### Check that something answers

```yaml
probes:
  - name: shop
    http: https://shop.example.com/health
    interval: 30s
    expect_status: [200]        # default: any 2xx
    expect_body: '"db":"up"'    # optional: a regular expression the answer must contain
  - name: database-port
    tcp: db.internal:5432
  - name: mail
    tls: mail.example.com:993   # anything that speaks TLS from the first byte
  - name: gateway
    ping: 10.0.0.1              # a host name or an address
```

| Check | It succeeds when |
|---|---|
| `http` | the address answers with a 2xx status (or one of `expect_status`), with a certificate that is valid for `https`, and with a body matching `expect_body` if set |
| `tcp` | the port accepts a connection |
| `tls` | the TLS handshake works and the certificate is valid for that host name |
| `ping` | at least one of three ICMP echo requests is answered |

Each takes `interval` (default: the agent's), `timeout` (default 10s) and
`labels`. `tls_insecure_skip_verify: true` accepts any certificate.

- **Certificates.** `https` and `tls` report the time left on the
  certificate as `probe_tls_cert_expiry_seconds`, also when the certificate
  is refused, so an expired one shows as expired. `tls` is for services
  that start with TLS (HTTPS, IMAPS, SMTPS on 465, LDAPS, a database with
  TLS only); it does not speak STARTTLS.
- **Ping** needs the right to send ICMP: an agent running as root has it,
  as has a container with `NET_RAW` (Docker's default). Another user needs
  the host to allow it (`net.ipv4.ping_group_range`). Without it the check
  fails. A name is resolved on every check, IPv4 preferred.
- **Where from.** A check runs on the agent whose config has it, so "does
  not answer" means "not from there". Give the same check to agents in two
  places to tell a site that is down from a network that is.

A failing check raises `ProbeFailing` after two minutes, a certificate
with less than 14 days left `CertificateExpiresSoon`, an expired one
`CertificateExpired`; these rules come with vigil ([Alerts](alerts.md)).
The **Site checks** dashboard shows all checks, how often each answered
and the days left on every certificate.

### Watch particular processes

```yaml
processes:
  - name: nginx                         # processes called nginx
  - name: order-service
    cmdline: 'java .*order-service'     # or: a regular expression on the command line
```

A group that matches nothing reports `process_group_count 0`; alert on that
to learn when something is not running.

### systemd services

```yaml
systemd:
  enabled: true
  units: [nginx.service, "postgresql*"]   # report these one by one
```

How many units are active, inactive and failed is always reported.

### Applications instrumented with OpenTelemetry

```yaml
otlp:
  enabled: true
  listen: 127.0.0.1:4318
```

Start the application with

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_SERVICE_NAME=checkout
```

Its metrics and logs arrive labelled `service="checkout"`, and its traces
feed the Services pages; see [Services and traces](apm.md) for sampling
(`otlp.traces`) and what is shown. gRPC (port 4317) is not supported yet.

### Limits

```yaml
limits:
  max_series: 100000                  # distinct metric series from this host
  max_log_bytes_per_second: 2097152   # averaged over a minute
```

What is over the limit is dropped on the agent and counted in
`vigil_agent_dropped_total`.

### Kubernetes and runtime events

See [Kubernetes](kubernetes.md) and [Runtime events](security.md).

## Checking a configuration

- The agent refuses to start with a file that has a mistake, and says what
  is wrong.
- The server refuses to save a profile that would give any agent an invalid
  configuration.
- On the **Fleet** page, **Config** on an agent shows exactly what it runs
  and where each setting comes from.
