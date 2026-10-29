# Dashboards

A dashboard is a grid of panels that share one time range, one refresh
interval and one set of variables.

A new server comes with **Hosts**, **Containers**, **Logs and security**,
**Site checks** (what answers, how fast, and the days left on
certificates), **vigil agents** (what each agent sends and what it costs
its host) and **AI costs** (all AI spending together: applications,
coding tools and GPU time, priced).
Integrations add their own the first time they are used (Kubernetes,
PostgreSQL, …). All of them are ordinary dashboards: change or delete them.

## Using one

- **Time range** and **Refresh** at the top apply to every panel. The range
  is a quick "last …" or an absolute From-To, with « and » to step through
  time; see [Explore → The controls](explore.md#the-controls).
- **Drag across any chart** to zoom the whole dashboard into that window.
- **Variables** are the drop-downs next to the title, for example `agent`.
  "All" shows everything.
- The address bar holds the range and the variable choices: copy it to share
  the view.

## Editing (admins)

**Edit**, then:

- **Add panel**, or **Edit** on a panel.
- Drag a panel by its title to move it: an orange bar shows whether it
  lands before or after the panel under the pointer. Panels fill the rows
  in order, left to right, so a panel cannot be left on its own in the
  middle of an empty row. ◀ ▶ do the same one step at a time.
- Drag a panel's bottom right corner to resize it, in twelfths of the
  width and in rows; the numbers are in the panel editor too.
- ✕ removes a panel.
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
| Map | what is connected to what, and its health (admins only) | set in the dashboard's JSON; below |

A panel has a **width** out of 12 (6 is half the row) and a **height** in
rows.

**Queries** are PromQL. Choose "LogsQL (logs)" to compute numbers from logs
instead; such a query has to end in `| stats …`:

```logsql
level:error | stats by (app) count() as errors
```

In a table, a LogsQL query with several results gets a column for each,
titled by the result's name:
`| stats by (path) count() as requests, avg(ms) as avg_ms`.

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

### Maps

A map draws nodes in columns, the links between them, and on hover a card
with a node's labels and details. Each query of a map is one of three
kinds, set in the dashboard's JSON ("Title, variables, JSON"); the panel
form edits the title and size and leaves the queries as they are.

| Query | Fields | Each series is |
|---|---|---|
| nodes | `node`, `tier`, optional `name`, `group`, `warn`, `crit` | a node: its id is the value of label `node`, drawn in the column `tier`, under the value of label `group`, named by label `name`. Its value is its health, coloured like a table cell |
| links | `from`, `to`, optional `live` | a link from the node named by label `from` to the one named by label `to`. At zero it is drawn faint; with `live`, a value above zero moves (sessions open now) |
| details | `of`, `legend`, optional `unit`, `warn`, `crit` | a line on the card of the node named by label `of`; its colour counts toward the node's |

A node is a label's name and value together: `gateway="gw-1"` and
`exported_agent="gw-1"` are two nodes. A node's colour is the worst of its
own and its details'. `columns` lists the labels its card shows. To join
nodes that have no label in common, give them one with
`label_replace(…, "control", "Boop", "", "")`. A map takes up to 20
queries and draws up to 300 nodes.

```json
{"title": "Map", "type": "map", "w": 12, "h": 6, "columns": ["hostname"], "queries": [
  {"expr": "gateway_info", "node": "gateway", "tier": "Gateways"},
  {"expr": "agent_connected * on (agent_id) group_left (gateway, hostname) agent_info",
   "node": "agent_id", "name": "hostname", "tier": "Agents", "warn": 0.5, "crit": 0},
  {"expr": "agent_info", "from": "gateway", "to": "agent_id"},
  {"expr": "cert_expiry_timestamp_seconds - time()", "of": "agent_id",
   "legend": "Certificate left", "unit": "seconds", "warn": 604800, "crit": 172800}]}
```

In this version only admins see maps; others see a note in their place.
The series behind a map are still there for anyone who may query them in
Explore.

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
