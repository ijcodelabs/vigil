# vigil

One monitoring system for metrics, logs, alerts, runtime security events and
dashboards: a single Go binary that runs as an agent on every host or
Kubernetes node (`vigil agent`) and as the central server (`vigil server`),
with VictoriaMetrics and VictoriaLogs as storage.

## What it does

- **Metrics**: host and container figures, Prometheus scraping (static
  lists, Kubernetes pods by annotation), probes, process groups, systemd
  units, OpenTelemetry (OTLP).
- **Logs**: files, journald, Docker containers, Kubernetes pods and events,
  with parsing templates.
- **Alerts**: Prometheus-format rules on metrics and on logs; Alertmanager
  built in for routing, contacts and silences.
- **Runtime events**: eBPF probes, security rules evaluated on the host, and
  captures of what happened around a detection.
- **Fleet**: what agents collect is managed on the server in profiles
  selected by labels, with integrations per technology and detection of
  what runs unmonitored.
- **Web interface**: Dashboards, Explore, Alerts, Captures, Fleet, Users.

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
