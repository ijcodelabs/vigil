# Explore

Explore is for asking questions of the data directly: **Metrics** with
PromQL, **Logs** with LogsQL. The address bar always holds the query and the
time range, so copying it shares exactly what you see.

## Saved queries

A query you will want again can be kept under a name, on both the Metrics
and the Logs tab.

- **Save** keeps the query that is in the box. If it came from one of your
  saved queries and you changed it, Save offers to update that one or to
  save a new one.
- **Saved** lists what there is, with a field to search names and queries.
  Click one (or press Enter for the first match) and it runs. While the
  query in the box is a saved one, the button shows its name.
- What is saved is the query, not the time range: picking one keeps the
  range you are looking at.

The list has up to four parts:

| Part | What is in it | Who can change it |
|---|---|---|
| **Yours** | what you saved | you. Viewers can save too; nobody else sees these, not even admins |
| **Shared** | what an admin saved with **Shared** ticked | admins. Everyone sees them |
| **Comes with vigil** | starter queries, and those of the [integrations](fleet.md#integrations) in use (marked with the integration's name) | nobody: they follow the version. Pick one, change it and save it as your own |
| **Recent** | the last 20 queries you ran that are not saved | kept in this browser only, per account |

**Edit** on a saved query renames it, changes its text or (admins) shares
it or takes it back. A shared query that is taken back becomes the query of
the admin who did it. Deleting an account deletes what it saved for itself;
what it shared stays.

A name is used once among your own queries, and once among the shared
ones, separately for metrics and logs. There can be 200 of your own and
1,000 shared.

Shared queries can also be kept as files in git and applied with `vigil
apply`; see [Fleet](fleet.md#keeping-profiles-in-git). Those are marked
"from files".

### From a query to a dashboard or an alert

**Add to** (admins) sends the query in the box on:

- **Dashboard…** opens the panel editor with the query filled in. Choose a
  dashboard, or a new one, and the panel is added at its end. A LogsQL
  query without `| stats` becomes a panel of log lines, one with it a
  table.
- **Alert rules…** makes an alert of it. For PromQL, say when it should
  alert ("is above 90"); for LogsQL, how many matching lines in how long.
  The expression that results can be edited, and the dialog runs it as
  you type and tells you how many alerts it would raise right now. The
  rule is written into the rule file, into the group you name, and the
  whole file is checked before anything changes, as when you
  [edit it by hand](alerts.md#rules). Comments and everything else in the
  file stay. If your rule file lives in git, copy the rule from **As
  YAML** instead of adding it here.

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

Most of these are under **Saved** already.

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
  to the query, so only lines with that value stay; **exclude** hides the
  lines with that value instead (it adds `-field:"value"`). Each undoes the
  other for the same value.
- **Live tail** shows matching lines as they arrive, newest on top.
- **Stop sending lines like this…** (admins, under the fields of an opened
  line) makes a rule so that the line's agent no longer sends such lines.
  It proposes a pattern from the line, with numbers and ids replaced by
  what they may be; edit it down to what the unwanted lines have in common.
  Before anything is saved it shows how many lines of that source in the
  last hour the pattern would have dropped and kept, with examples of both.
- **Context** (under the fields of an opened line) shows what you would
  otherwise look up by hand before deciding whether a line matters; see
  [A line's context](#a-lines-context). **Explain…** (admins, with a
  language model set up) asks the model about it, from that context.
- The newest lines up to the chosen limit are shown.

### A line's context

**Context** gathers, without any AI:

- **Lines like it**: lines of the same source (the container, pod, unit or
  app) that match a pattern made from this one, with numbers and ids
  replaced, counted in the last hour, 24 hours and 7 days, when the first
  and last came, and on which agents. **show them** opens them in Explore.
- **Around it**: up to ten lines before and after it in the same stream,
  the line itself highlighted.
- **On the host, 10 minutes either side**: the vigil agent starting,
  containers stopping or starting, container restarts, out-of-memory kills,
  and profile changes; and the incidents of that host open at the time.
- **What stands out**, said plainly, for example: the text says *error*
  but the line's level is *info*; it is the only line like it in a week;
  the agent started in the same second (a request it had open was cut off
  by its own restart).

**Explain…** sends that context to the language model, after showing
exactly what would be sent, the same way as for an incident (see
[Home → AI explanations](home.md#ai-explanations)). The answer says whether
the line is **harmless**, **needs attention** or is **unclear**, what it
means, why it happened and what to check, each fact citing the evidence it
rests on. A line is log content, so this needs `data: [logs]` in the
server's `ai` settings; without it nothing is sent. Lines like each other
(same pattern, source and host, on the same day) share one explanation:
asking about the next one shows the earlier answer at no cost, and
**Explain again…** makes a new one.

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

Several of these are under **Saved** already.

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

## Log volume

**Explore → Log volume** answers "who writes all these logs": lines and
size per container, pod, systemd unit or application in the chosen time
range, biggest first, with each one's share. **show lines** opens that
source in Logs.

An admin can **stop collecting** the logs of a container, a systemd unit,
a pod or an application on that agent:

- The change is written into the profile of that one agent,
  `agent-<name>`: for a container as `docker.container_logs`, for the
  others as a `logs.drop` rule without a pattern. It is saved like any
  profile: checked first, with a version you can go back to.
- The agent picks it up within a minute. Lines already stored stay until
  they age out; the container's metrics are not affected.
- What is switched off is listed under **Switched off**, with **collect
  again** to take it back.
- **Lines that are dropped** lists every drop rule, also those made from
  a line under Logs, with **remove**.

If your profiles are files in git ([Fleet](fleet.md)), make the same change
there: `vigil apply -prune` removes profiles the files do not have. A
pattern for several containers, or a rule for several agents, goes into a
profile by hand ([Agent configuration](configs.md)).

## From Explore to elsewhere

- A query you will type again: **Save** it.
- A query that is worth looking at every day belongs on a
  [dashboard](dashboards.md): **Add to → Dashboard…**
- A query that should wake someone belongs in a [rule](alerts.md):
  **Add to → Alert rules…** Rules use the same two languages.
