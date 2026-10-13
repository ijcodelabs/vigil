# Dashboards

A dashboard is a grid of panels that share one time range, one refresh
interval and one set of variables.

A new server comes with **Hosts**, **Containers**, **Logs and security**,
**Site checks** (what answers, how fast, and the days left on
certificates), **vigil agents** (what each agent sends and what it costs
its host) and **AI costs** (language-model tokens and GPU time, priced).
Integrations add their own the first time they are used (Kubernetes,
PostgreSQL, …). All of them are ordinary dashboards: change or delete them.

## Using one

- **Time range** and **Refresh** at the top apply to every panel.
- **Drag across any chart** to zoom the whole dashboard into that window.
- **Variables** are the drop-downs next to the title, for example `agent`.
  "All" shows everything.
- The address bar holds the range and the variable choices: copy it to share
  the view.

## Editing (admins)

**Edit**, then:

- **Add panel**, or **Edit** on a panel.
- ◀ ▶ move a panel earlier or later; ✕ removes it.
- **Title, variables, JSON** changes the title and the variables, and shows
  the whole dashboard as JSON.
- Nothing is stored until **Save**. **Discard changes** goes back.

A panel can also come from [Explore](explore.md#from-a-query-to-a-dashboard-or-an-alert):
**Add to → Dashboard…** there puts the query on screen on a dashboard, at
its end, and saves it at once.

### Panels

| Shows | Use for | Options |
|---|---|---|
| Lines over time | how something develops | unit, where the Y axis starts, fill under lines, a legend text per query |
| Single number | one figure to glance at | unit, "yellow from" and "red from" thresholds |
| Table | the current value of many series, with one column per query | unit, which labels are columns; per query a column title, a unit, and "yellow from" and "red from" |
| Log lines | the latest matching lines | number of lines |

A panel has a **width** out of 12 (6 is half the row) and a **height** in
rows.

**Queries** are PromQL. Choose "LogsQL (logs)" to compute numbers from logs
instead; such a query has to end in `| stats …`:

```logsql
level:error | stats by (app) count() as errors
```

**Legend** names each line, with labels in double braces:
`{{agent}} in`, `{{namespace}}/{{pod}}`.

**Units**: percent (0–100), ratio (0–1, shown as percent), bytes, bytes per
second, seconds, milliseconds, CPU cores, per second, yes / no (1 / 0).

**Tables with several columns.** Every query of a table is a column, named
by the query's legend. Rows are the series, matched across the queries by
their labels, so `probe_success` and `probe_duration_seconds` of the same
check land in one row; a query that has nothing for a row leaves the cell
empty. The first query decides the order of the rows. "Label columns"
picks which labels are shown, and in what order.

A column can have its own unit and thresholds: a cell is yellow from one
value and red from another. If the red value is below the yellow one, low
values are the bad ones (a certificate with 7 days left, an availability
of 99%).

### Variables

A variable is a drop-down listing the values of one label:

| Field | Example | Meaning |
|---|---|---|
| name | `agent` | used in queries as `$agent` |
| label | `agent` | the label whose values are listed |
| series selector | `host_load1` | optional: only values from matching series |

Use it in queries so that "All" works:

```promql
host_load1{agent=~"$agent"}
```

```logsql
agent:~"$agent" level:error
```

## Copying a dashboard

**Title, variables, JSON → Edit as JSON** shows the dashboard as text. Paste
that into a new dashboard on another server. Through the API:

```bash
curl -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://vigil/api/v1/dashboards/hosts > hosts.json
curl -X PUT -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" --data-binary @hosts.json https://other/api/v1/dashboards/hosts
```

## Limits to know

- Panels are placed by width, height and order; there is no dragging or
  resizing with the mouse.
- Variables hold one value or "All".
- Grafana dashboards cannot be imported; the format is vigil's own.
- A chart of counted log lines shows a gap, not zero, where nothing matched.

## The shown time inside a query

`$__range` in a query is replaced by the length of the time the dashboard
shows (`3600s` for "Last 1h"), and `$__range_s` by the same as a plain
number of seconds. Use it on single-number and table panels for
totals over what is shown:

```
sum(increase(http_requests_total[$__range]))
```

## Dashboards that come with vigil

The starter dashboards and the dashboard of every integration in use are
installed once and are ordinary dashboards from then on. The list shows
where each came from ("starter", "integration haproxy").

When you upgrade vigil, or change an integration of your own, such a
dashboard may have a newer version:

- **You never changed it**: it is replaced with the newer version.
- **You changed it**: it is left as it is. The list marks it "newer version
  available" and the dashboard shows a notice, with three choices:
  **Update** (replace yours), **Update, keep mine as a copy** (yours stays
  as a second dashboard, "… (mine)"), or **Keep mine** (the notice goes
  away until there is yet another version). After **Update** the dashboard
  follows new versions by itself again.
- **You deleted it**: it stays deleted. To get it back:

  ```bash
  curl -X POST -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" -d '{"action":"restore"}' \
       https://vigil.example.com/api/v1/dashboards/haproxy/update
  ```

A dashboard installed by a vigil older than this feature counts as changed
unless it is identical to the current version, since nothing recorded what
was installed; expect the notice once on those.
