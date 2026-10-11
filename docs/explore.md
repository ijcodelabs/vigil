# Explore

Explore is for asking questions of the data directly: **Metrics** with
PromQL, **Logs** with LogsQL. The address bar always holds the query and the
time range, so copying it shares exactly what you see.

## Metrics

### Finding what there is

**Show metrics** lists every metric that has data in the chosen time range,
grouped by its first word (`host_`, `container_`, `kube_`, `probe_`, …).
Type to filter; click a name to graph it. The table under the chart then
shows the labels on each series, which is how you find out what you can
filter on. [Metric reference](metrics.md) says what each one means.

### The controls

- **Enter** runs the query; Shift+Enter adds a line.
- **Time range**: the last 5 minutes up to 7 days.
- **Drag across the chart** to zoom into that window. "Back to last 1h"
  returns.
- **Refresh** re-runs the query on a timer.
- Up to 60 series are drawn. If the query returns more, narrow it.

### PromQL in five minutes

A metric name selects every series of that metric:

```promql
host_load1
```

Braces filter by label. `=` equals, `!=` differs, `=~` matches a regular
expression (the whole value), `!~` does not match:

```promql
host_load1{env="prod"}
host_filesystem_free_bytes{agent=~"db-.*", mountpoint="/"}
```

Counters (names ending in `_total`) only ever go up, so you almost always
want their rate of change per second, over a window:

```promql
rate(host_network_receive_bytes_total[5m])
```

Aggregate across series with `sum`, `avg`, `max`, `min`, `count`, keeping
the labels after `by`:

```promql
sum by (agent) (rate(host_network_receive_bytes_total[5m]))
```

Arithmetic works between series with the same labels:

```promql
100 * (1 - host_memory_available_bytes / host_memory_total_bytes)
```

### Queries to start from

| Question | Query |
|---|---|
| Which agents are reporting? | `vigil_agent_up` |
| CPU used, percent, per host | `100 * (1 - sum by (agent) (rate(host_cpu_seconds_total{mode="idle"}[5m])) / sum by (agent) (rate(host_cpu_seconds_total[5m])))` |
| Memory used, percent | `100 * (1 - host_memory_available_bytes / host_memory_total_bytes)` |
| Fullest filesystems | `topk(10, 100 * (1 - host_filesystem_free_bytes / host_filesystem_size_bytes))` |
| Network in, bytes per second | `sum by (agent) (rate(host_network_receive_bytes_total[5m]))` |
| Is the host short of memory? | `host_pressure_ratio{resource="memory", kind="some"}` |
| Scrape targets that are down | `up == 0` |
| Containers using most memory | `topk(10, container_memory_usage_bytes)` |
| CPU cores used per container | `rate(container_cpu_usage_seconds_total[5m])` |
| Pods not running | `kube_pod_status_phase{phase!="Running"}` |
| Containers restarting | `increase(kube_pod_container_status_restarts_total[1h]) > 0` |
| Probes that fail | `probe_success == 0` |
| Days left on certificates | `probe_tls_cert_expiry_seconds / 86400` |
| Things that are not running | `process_group_count == 0` |
| Alerts firing now | `ALERTS{alertstate="firing"}` |
| Agents dropping data | `increase(vigil_agent_dropped_total[1h]) > 0` |

### Things that surprise people

- **The last 30 seconds are not shown.** The metric store hides the most
  recent half minute so that late-arriving data does not make charts jump.
- **A series stays visible for about 5 minutes after it stops.** A host
  that went away keeps its last value for that long.
- **`rate()` needs a window at least twice the collection interval.** With
  a 30-second interval, use `[1m]` or more, or the result is empty.

## Logs

### The controls

- **Enter** runs the query. An empty query or `*` shows everything.
- The **bar chart** shows how many lines matched over time.
- **Click a line** to see all its fields. **filter** next to a field adds it
  to the query.
- **Live tail** shows matching lines as they arrive, newest on top.
- The newest lines up to the chosen limit are shown.

### What a log line has

Every line has the message, a time, and fields. Some fields are **labels**
(they identify where the line came from and are fast to filter on):

| Label | Present on |
|---|---|
| `agent` | everything: the host that sent it |
| your agent labels (`env`, `role`, `cluster`, …) | everything |
| `source` | `file`, `journald`, `docker`, `kubernetes`, `kubernetes-events`, `otlp`, `security` |
| `app` | file sources that set it; pods with an `app` label |
| `path` | file sources |
| `unit` | journald |
| `container`, `image` | Docker containers |
| `namespace`, `pod`, `container` | Kubernetes pods |
| `service` | OpenTelemetry |
| `rule`, `severity` | security detections |

Whatever a parsing template extracted (`level`, `status`, `method`, …) is
there too.

### LogsQL in five minutes

Words find lines containing them; several words must all be present:

```logsql
error timeout
```

Quotes search for a phrase:

```logsql
"connection refused"
```

`field:value` filters on a field; `field:~"regex"` on a pattern; a leading
`-` excludes:

```logsql
app:nginx status:~"5.." -path:"/healthz"
source:kubernetes namespace:shop level:error
```

`or` between alternatives, with brackets:

```logsql
namespace:shop (level:error or level:warn)
```

After a `|`, lines can be counted and grouped:

```logsql
level:error | stats by (app) count() as errors | sort by (errors desc) | limit 10
```

### Queries to start from

| Question | Query |
|---|---|
| Everything from one host | `agent:web-01` |
| Errors anywhere | `level:~"(?i)^(err\|crit\|fatal)"` |
| One container's output | `source:docker container:shop-api` |
| One pod's errors | `namespace:shop pod:~"checkout-.*" error` |
| Failed SSH logins | `unit:sshd.service "Failed password"` |
| Kernel killing processes | `source:journald "Out of memory"` |
| Which apps log most? | `* \| stats by (app) count() as lines \| sort by (lines desc)` |
| Slow nginx responses by path | `app:nginx status:~"5.." \| stats by (path) count() as n \| sort by (n desc) \| limit 20` |
| Kubernetes warnings | `source:kubernetes-events type:Warning` |
| Why was a pod not scheduled? | `source:kubernetes-events reason:FailedScheduling` |
| Security detections | `source:security` |
| Lines of one trace | `trace_id:4bf92f3577b34da6a3ce929d0e0e4736` |

### Things that surprise people

- **Search is by whole words.** `time` does not find `timeout`; use
  `timeout`, or `~"time.*"` for a pattern.
- **A query with `| stats` returns a table, not lines**, so the bar chart
  and live tail do not apply to it.
- Field names and values are case-sensitive; use `(?i)` in a pattern to
  ignore case.

## From Explore to elsewhere

- A query that is worth keeping belongs on a [dashboard](dashboards.md).
- A query that should wake someone belongs in a [rule](alerts.md). Rules
  use the same two languages.
