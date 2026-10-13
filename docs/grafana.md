# Grafana

vigil has its own dashboards and Explore. If you would rather look at the
data in Grafana, or have one already, point it at the vigil server: the
server answers Grafana's data sources for metrics, logs, traces and alerts.
Agents, profiles, rules and who is told about alerts stay in vigil.

Grafana talks to the server, not to the stores behind it, so the stores
stay private and one token decides what Grafana may read.

## 1. A read-only token

**Users → Read-only tokens → New token.** The token is shown once. Whoever
has it can read what a viewer can see and change nothing; **Revoke** ends
it at once. The list shows when each token was last used.

Do not give Grafana the admin token: it can change everything.

## 2. The data sources

`deploy/grafana/datasources.yaml` in the repository sets up all four. Put
it in Grafana's `provisioning/datasources` directory and start Grafana with
two environment variables and the logs plugin:

```bash
docker run -d --name grafana -p 3000:3000 \
  -e VIGIL_URL=https://vigil.example.com \
  -e VIGIL_TOKEN=vgr_… \
  -e GF_INSTALL_PLUGINS=victoriametrics-logs-datasource \
  -v $PWD/deploy/grafana:/etc/grafana/provisioning/datasources:ro \
  grafana/grafana
```

By hand, under **Connections → Data sources**, the same four are:

| Data source | Type | URL |
|---|---|---|
| metrics | Prometheus | the server's address |
| logs | VictoriaLogs (plugin `victoriametrics-logs-datasource`) | the server's address |
| traces | Jaeger | the server's address + `/api/v1/traces/jaeger` |
| alerts | Alertmanager, implementation Prometheus | the server's address + `/alertmanager` |

For each one add a custom HTTP header: name `Authorization`, value `Bearer`,
a space, and the token. No other authentication.

If the server uses its own certificate authority (`tls.self_signed`), give
each data source that authority's certificate (`GET /api/v1/ca`) as its CA
certificate.

## Straight to the stores instead

A Grafana on the same host or Docker network as the stores can also ask
them directly, without vigil in between and without a token:

| Data source | Type | URL, with the names from `deploy/docker-compose.yml` |
|---|---|---|
| metrics | Prometheus | `http://victoriametrics:8428` |
| logs | VictoriaLogs plugin | `http://victorialogs:9428` |
| traces | Jaeger | `http://victoriatraces:10428/select/jaeger` |

Think before choosing this:

- The stores do not ask who is calling. Whoever reaches those ports reads
  everything and, on the same ports, can write and delete. Keep them on a
  private network; never publish them.
- vigil's accounts and tokens play no part, and revoking a token stops
  nothing.
- Alerts and silences are not in the stores; for those Grafana still needs
  the server's `/alertmanager`.

Through the server is the way that was tested and the one to prefer. Of
the three above, only the logs one was tried.

## What works

- **Metrics:** PromQL in Explore and in panels, with the metric and label
  pickers. The store is VictoriaMetrics, so its extra functions work as
  well; Grafana's editor may underline the ones Prometheus does not have.
- **Logs:** LogsQL in Explore and in panels: log lines, and numbers over
  time with `| stats …`.
- **Traces:** the server answers the Jaeger API that data source speaks,
  if it has a trace store. Without one the data source says "501 Not
  Implemented".
- **Alerts:** firing alerts and silences can be seen. The rules themselves
  show in the Prometheus data source. Making a silence from Grafana is
  refused, since the token only reads; make it in vigil.

## What stays in vigil

- Agents, profiles, integrations and rollouts (Fleet).
- Rules, contacts and silences (Alerts).
- Choosing what agents send: Log volume and drop rules (Explore).
- Captures of runtime events.
- vigil's dashboards are not copied to Grafana; Grafana starts empty.

## Good to know

- Every series and log line has the label `agent`; your agents' own labels
  (`env`, `role`, …) are there too. They make good dashboard variables.
- The last 30 seconds of metrics are not shown, in Grafana as in vigil.
- Grafana's queries count against nothing but the stores themselves: a
  heavy Grafana dashboard on a short refresh is load on the server's host.
