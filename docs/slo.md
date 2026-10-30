# Service level objectives

An SLO says what share of something has to go well over a period: "99.9 %
of requests to the shop API answer without a server error, over 30 days".
vigil counts, once a minute, how many events there were and how many were
good, and keeps both as metrics. The error budget, burn rates and reports
are worked out from those two numbers.

Not built yet: burn-rate alerts, an SLOs page in the interface and monthly
reports. Until then the queries below show the same in Explore or on a
dashboard.

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
labels: {tier: "1"}           # optional, on the recorded series
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

Admins save SLOs; everyone signed in can read them.

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
