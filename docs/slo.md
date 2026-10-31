# Service level objectives

An SLO says what share of something has to go well over a period: "99.9 %
of requests to the shop API answer without a server error, over 30 days".
vigil counts, once a minute, how many events there were and how many were
good, and keeps both as metrics. The error budget, burn rates and reports
are worked out from those two numbers.

Each SLO also alerts when its budget goes too fast (see
[Alerts](#alerts)), the [SLOs page](#the-slos-page) shows where each
one stands, and [SLA reports](#sla-reports) say what each achieved in a
month or quarter.

## An SLO

```yaml
name: shop-api-availability   # lower-case letters, digits, dashes; it is the label `slo`
service: shop-api             # optional; label `service`
owner: team-shop              # optional; label `owner`
description: Requests to the shop API answer without a server error.
objective: 99.9               # percent, above 50 and below 100
window: 30d                   # 7d, 28d, 30d (the default) or calendar_month
indicator:
  span:
    service: shop-api
alerting:                     # optional; both kinds are on by default
  page: true                  # SLOBurnFast, critical
  ticket: true                # SLOBurnSlow and SLOBudgetExhausted, warning
  min_events: 10              # per hour; fewer and no burn-rate alert (not for probe SLOs)
labels: {tier: "1"}           # optional, on the recorded series and the alerts
runbook_url: https://wiki.example.com/shop-api
```

### What counts as an event

Exactly one of these under `indicator`:

**`probe`**: every check of a [site check](configs.md) is an event, a
successful one is good. All agents that run the probe count together;
`agents` limits it to some.

```yaml
indicator:
  probe: {name: shop-home, agents: [edge-1, edge-2]}
```

**`span`**: every span of a service is a request, from the
[span counters](apm.md) the agent keeps for every span before sampling, so
sampling does not change the numbers. A span with an error status is bad.
With `latency_below`, a slower one is bad too; the bound must be one of
the histogram's bounds (5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s,
2.5s, 5s, 10s, 30s), and another value is refused with the two nearest.

```yaml
indicator:
  span:
    service: shop-api
    kind: server                          # the default; or consumer, client, producer, internal
    span_name: ["GET /cart", "POST /checkout"]   # optional; every span of the kind otherwise
    latency_below: 500ms
```

**`ratio`**: any counters, for exporters and your own metrics. `total` and
either `good` or `bad`, each a series selector: a counter's name with
optional label matchers. vigil takes their increase per minute itself, so
write no `rate()` or `sum()`.

```yaml
indicator:
  ratio:
    bad:   haproxy_backend_http_responses_total{proxy="shop", code="5xx"}
    total: haproxy_backend_http_responses_total{proxy="shop"}
```

## Saving one

Admins save SLOs on the [SLOs page](#the-slos-page) (**New SLO**, or
**Edit** on an SLO's page) or through the API; everyone signed in can
read them.

```bash
curl -X PUT -H "Authorization: Bearer $T" --data-binary @shop-api.yaml https://vigil/api/v1/slos/shop-api-availability
```

The metric store checks the queries made of it first, and a refused one is
not saved. The answer lists warnings that do not stop the save, such as
"no events in the last hour from the probe "shop-home"": usually a
misspelt name. Every save keeps the previous version
(`GET /api/v1/slos/{name}/versions`).

Kept in git, one SLO per file in a `slos` directory next to your profiles:

```
fleet/
  base.yaml
  slos/
    shop-api.yaml
```

```bash
vigil apply -f fleet/            # -prune also deletes SLOs that came from files and are gone
```

## Templates

**From a template** on the SLOs page (admins) offers an SLO for what
reported in the last day, as a document to look at and change before it
is saved; nothing is made by itself.

| Template | One for each | Objective |
|---|---|---|
| Site check succeeds | check (`probe`) | 99.5 % of runs |
| Service answers without errors | traced service with server spans | 99.9 % of requests |
| Service answers fast | traced service | 99 % within 500 ms |
| HAProxy: backend answers without server errors | backend, while the HAProxy integration is in use | 99.9 % of responses not 5xx |

nginx has no template: its status codes are only in its logs, and SLOs
read metrics. Your own integrations can ship templates (`slos:`, see
[Fleet](fleet.md#your-own-integrations)). An offer whose SLO name exists
links to that SLO instead.

`GET /api/v1/slos/templates` (admins) returns the same offers.

## What is recorded

Each SLO runs as a rule group `slo:<name>` (under Alerts > Rules, not
editable there) and writes, once a minute:

| Metric | Value |
|---|---|
| `slo_total_per_minute{slo, service, owner}` | events in that minute |
| `slo_good_per_minute{slo, service, owner}` | good events in that minute |
| `slo_objective{slo, service, owner}` | the objective as a ratio (`0.999`) |

A minute is counted 90 seconds after it ends, so that late samples are in,
and each minute exactly once: saving rules or restarting the server does
not count one twice. Minutes the server missed (a restart, a store that
did not answer) are counted late, up to 30 of them; older ones stay gaps,
counted as neither good nor bad. A minute without events records nothing.

## Alerts

Each SLO runs a second rule group, `slo:<name>:alerts`, with the usual
multi-window, multi-burn-rate alerts (from Google's SRE workbook). The
burn rate is how fast the budget goes: 1 spends exactly the budget in one
window. An alert fires when a long window and a short one both burn faster
than the rate at which a given share of the budget would be gone:

| Alert | Severity | Long window | Short window | Fires when it would spend | Rate for 30 days |
|---|---|---|---|---|---|
| `SLOBurnFast` | critical | 1 hour | 5 minutes | 2 % of the budget in that hour | 14.4 |
| `SLOBurnFast` | critical | 6 hours | 30 minutes | 5 % in six hours | 6 |
| `SLOBurnSlow` | warning | 1 day | 2 hours | 10 % in a day | 3 |
| `SLOBurnSlow` | warning | 3 days | 6 hours | 10 % in three days | 1 |
| `SLOBudgetExhausted` | warning | the SLO's window | | the whole budget | |

- The long window says it is real, the short one that it is still going
  on, so an alert ends soon after the cause does.
- The rates follow the window: a 7-day SLO pages at 3.36 instead of 14.4,
  for the same 2 % of its budget in an hour.
- A burn-rate alert waits until the SLO has counted for three quarters of
  its long window (45 minutes for the hourly one), and
  `SLOBudgetExhausted` for a quarter of the SLO's window: a new SLO does
  not alert off its first few minutes.
- `min_events`: while the long window holds fewer events than that per
  hour (10 by default), no burn-rate alert. Probe SLOs check at a known
  rate and have no minimum.
- The alerts read the counted minutes up to two minutes ago, so they come
  about four minutes after the events (90 s for counting, 2 minutes to
  leave out the minute being written).
- A calendar-month SLO has the burn-rate alerts but no
  `SLOBudgetExhausted`.
- An objective so low that a page would need more than every event to fail
  (90 % over 30 days, say) is saved with a warning that the page can never
  fire.

Alerts carry `slo`, `service`, `owner`, `severity` and the SLO's labels,
and the annotations `summary` (with the burn rate), `objective`,
`description` and `runbook_url`. Route them like any other alert
([Alerts](alerts.md)), by `slo` or `owner`. On the [Home](home.md) page each
is an incident of kind SLO with a link to its burn rate over three days.
These alerts use MetricsQL's `tfirst_over_time`, so they need
VictoriaMetrics as the metric store.

## The SLOs page

**SLOs** in the menu (shown to admins, and to everyone once there is an
SLO) lists every SLO with its objective, what it achieved over its
window, the error budget left, the burn rate over the last hour, the
coverage and the alerts that fire. Above three SLOs a filter finds them by
name, service, owner or label.

An SLO's page has:

- **Achieved**, **Error budget left** (with how many more bad events the
  budget allows at the window's volume), the **burn rates** over 1 hour,
  6 hours, 1 day and 3 days, and the **coverage**: the share of minutes
  counted since the SLO started counting. A burn rate is red above the
  rate its alert fires at.
- **Error budget left** over the window, with a straight line for a budget
  spent evenly: above the line is better than even. For a calendar month
  the line runs to the end of the month.
- The **bad share** and the number of **events**, per hour (per three
  hours for windows over ten days).
- For a span SLO, **where the budget goes**: the operations with bad
  events in the last 24 hours.
- The SLO's **incidents** in the window, its silences, its **history**
  (show and restore an earlier version; admins), and the **generated
  rules**.

Figures are read two minutes behind, like the alerts, so the page and the
alerts agree. On the Home page, an SLO with less than a quarter of its
budget left is under "Coming up" (for a calendar month, while more than a
week of the month is left); an exhausted budget is an alert instead.

The starter dashboard **SLOs** shows every SLO's figures over 30 days and
its bad share, burn rate and events over time, with a variable `slo`.

## SLA reports

**Reports** on the SLOs page shows a month or a quarter: for each SLO its
objective, what it achieved, met or missed, the share of the error budget
used, the bad events, the worst day (the finished UTC day with the lowest share
of good events), the coverage, and its incidents
with how long they lasted and who acknowledged them. **CSV** downloads it
for a spreadsheet; **Print or PDF** prints the page alone (choose "Save as
PDF" in the print dialog) for people without a vigil account.

- Periods are calendar months and quarters in UTC, whatever an SLO's own
  window is.
- A finished period's report is worked out once and kept: a later change
  to an SLO, to the incidents kept or to the metric store's retention does
  not change a report already given. vigil stores last month's and last
  quarter's report by itself within an hour after they end. The current
  month is shown as **not final** and is worked out anew each time.
- The objective is the SLO's when the report is made; a `*` marks an SLO
  whose objective changed during the period.
- An SLO that counted nothing in the period (made after it, say) is left
  out. One that started during the period has a coverage below 100 %.
- vigil says what it measured. Service credits and contract terms are not
  part of it.

Scripts fetch the same with any token, a read-only `vgr_` one included:

```bash
curl -H "Authorization: Bearer $VIGIL_READ_TOKEN" "https://vigil/api/v1/slos/report?period=2026-10"
curl -H "Authorization: Bearer $VIGIL_READ_TOKEN" "https://vigil/api/v1/slos/report?period=2026-Q4&format=csv"
```

The metric store must keep the SLO series for the whole period when the
report is made: a quarter needs at least 93 days of retention, unless its
report is stored right after the quarter ends (which vigil does).

## Queries

Error ratio, budget left and burn rate over the window (here 30 days):

```promql
# share of bad events
1 - sum by (slo) (sum_over_time(slo_good_per_minute[30d])) / sum by (slo) (sum_over_time(slo_total_per_minute[30d]))

# budget left: 1 is untouched, 0 spent, below 0 overspent
1 - (1 - sum by (slo) (sum_over_time(slo_good_per_minute[30d])) / sum by (slo) (sum_over_time(slo_total_per_minute[30d])))
  / (1 - max by (slo) (slo_objective))

# burn rate over the last hour: 1 spends the budget in exactly one window
(1 - sum by (slo) (sum_over_time(slo_good_per_minute[1h])) / sum by (slo) (sum_over_time(slo_total_per_minute[1h])))
  / (1 - max by (slo) (slo_objective))

# coverage: share of the window's minutes that were counted
count_over_time(slo_total_per_minute[30d]) / (30 * 24 * 60)
```

Add the counts up before dividing, as above: an average of ratios (per
minute, per agent, per region) weighs a quiet minute like a busy one.

Keep the metric store's retention above the window: 30 days of SLO need
at least 31 days of data.
