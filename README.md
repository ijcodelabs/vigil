# vigil

Monitoring for servers and clusters in one Go binary: metrics, logs,
alerts, site checks, traces and runtime security events, with the
dashboards to look at them.

It is for people who would otherwise wire an agent, a log shipper,
Alertmanager, a dashboard tool and a runtime-security tool together. The
same binary runs as the agent on every host or Kubernetes node
(`vigil agent`) and as the central server (`vigil server`).
VictoriaMetrics and VictoriaLogs store the data.

A new server is useful before you configure anything: it alerts when a host
goes silent, a container stops, a site stops answering or a certificate is
about to expire, and it comes with dashboards for hosts, containers, site
checks and logs.

**Status: early.** It runs on its author's servers and a home lab, not on
large fleets. Expect rough edges, and please say what you find.

![The Containers dashboard: running containers, memory and CPU in use, and charts per container](docs/img/containers.png)

| | |
|---|---|
| [![A service page: request rate, duration, operations and dependencies](docs/img/service.png)](docs/img/service.png) **Services** from OpenTelemetry traces | [![A failed request as a trace, span by span](docs/img/trace.png)](docs/img/trace.png) **Traces**, span by span |
| [![Log search with a histogram of matching lines](docs/img/logs.png)](docs/img/logs.png) **Logs** from files, journald and containers | [![The agent list with labels, profiles and what runs unmonitored](docs/img/fleet.png)](docs/img/fleet.png) **Fleet**: agents configured from the server |

## What it does

- **Metrics**: host and container figures, Prometheus scraping (static
  lists, Kubernetes pods by annotation), process groups, systemd units,
  GPUs, OpenTelemetry (OTLP).
- **Logs**: files, journald, Docker containers, Kubernetes pods and events,
  with parsing templates. A log volume page shows who writes the most, and
  lines not worth keeping are dropped by a rule you preview first.
- **Site checks**: HTTP, TCP, TLS and ping from any agent, with the time
  left on certificates and where the time of a request goes.
- **Alerts**: Prometheus-format rules on metrics and on logs, with the
  essential ones built in; Alertmanager inside for routing, contacts and
  silences, and a message to the contacts when someone silences an alert.
- **Traces**: OpenTelemetry traces with request rate, errors and duration
  per service, and a view of each trace.
- **Runtime events**: eBPF probes, security rules evaluated on the host, and
  captures of what happened around a detection.
- **Fleet**: what agents collect is managed on the server in profiles
  selected by labels, which can be kept in git, with integrations per
  technology and detection of what runs unmonitored.
- **Web interface**: Dashboards, Explore, Services, Alerts, Captures, Fleet.

## Documentation

The guides are in [`docs/`](docs/README.md), and the server shows the same
pages under `/docs/` (the **Docs** link in the interface).

| | |
|---|---|
| [Install](docs/install.md) | server, agents, tokens |
| [Agent configuration](docs/configs.md) | every source, with recipes |
| [Fleet](docs/fleet.md) | labels, profiles, integrations, `vigil apply` |
| [Kubernetes](docs/kubernetes.md) | node agents and the cluster agent |
| [Explore](docs/explore.md) | PromQL and LogsQL with examples |
| [Dashboards](docs/dashboards.md) | panels, variables, editing |
| [Alerts](docs/alerts.md) | rules, contacts, silences |
| [Runtime events](docs/security.md) | events, rules, captures |
| [Metric reference](docs/metrics.md) | what every metric means |
| [API](docs/api.md) | endpoints |
| [Troubleshooting](docs/troubleshooting.md) | when something does not work |

## Build

    make build     # bin/vigil for this machine
    make linux     # static bin/vigil-linux-{amd64,arm64}
    make test
    make bpf       # only after changing internal/events/bpf/events.c (needs podman or docker)

Building needs only Go: the compiled kernel probes and the interface's two
small JavaScript libraries are committed.

## Quick start

    # central node: server + VictoriaMetrics + VictoriaLogs
    printf 'VIGIL_INGEST_TOKEN=%s\nVIGIL_ADMIN_TOKEN=%s\n' "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" > deploy/.env
    docker compose -f deploy/docker-compose.yml up -d

Open `http://localhost:8080`, sign in as `admin` with the admin token, and
use **Fleet → Add agent** to get the file for a first host. The rest is in
[Install](docs/install.md).

## For contributors

`memory/context/` records what is built, why it is built that way, where the
project is going and how to test it against real stores, a real kernel and a
real cluster. Read it before changing things.

## License

GNU Affero General Public License v3.0; see [LICENSE](LICENSE). The kernel
probes in `internal/events/bpf/events.c` are dual BSD/GPL so the kernel
accepts them.
