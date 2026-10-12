# API

Everything the interface does goes through this API, on the server's
address.

## Signing requests

```bash
curl -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://vigil.example.com/api/v1/agents
```

- The **admin token** may do everything.
- A browser session (cookie) works too; requests that change something must
  then carry the header `X-Vigil-CSRF: 1`.
- **viewer** accounts may read; **admin** accounts may change.
- Agents use their own token or the shared ingest token, and only on the
  ingest endpoints.

Errors come back as plain text with a 4xx or 5xx status.

## Querying

| | |
|---|---|
| `GET` `POST /api/v1/query`, `/query_range`, `/series`, `/labels`, `/label/{name}/values` | the Prometheus query API, answered by the metric store |
| `GET` `POST /api/v1/logs/query` | LogsQL; one JSON object per line. Parameters `query`, `start`, `end`, `limit` |
| `…/logs/hits` | counts over time (`step`) |
| `…/logs/stats_query`, `…/logs/stats_query_range` | numbers computed from logs, in Prometheus' response format |
| `…/logs/tail` | a stream of matching lines as they arrive |
| `…/logs/streams`, `stream_field_names`, `stream_field_values`, `field_names`, `field_values` | what there is to filter on |

```bash
curl -H "Authorization: Bearer $T" --data-urlencode 'query=up == 0' https://vigil/api/v1/query
curl -H "Authorization: Bearer $T" --data-urlencode 'query=level:error' --data-urlencode 'limit=20' https://vigil/api/v1/logs/query
```

Traces: `GET /api/v1/traces/jaeger/api/…` (the trace store's Jaeger query
API) and `GET` `POST /api/v1/traces/query` (LogsQL over spans); see
[Services and traces](apm.md#finding-traces-yourself).

## Rules and alerts

| | |
|---|---|
| `GET` `PUT /api/v1/rules/config` | the rule file (YAML). `PUT` replaces it; refused with the reason if invalid |
| `GET /api/v1/rules` | groups, each rule's health and alerts |
| `GET /api/v1/rules/builtin` | the rules that come with vigil (YAML), as they are in the running version |
| `GET /api/v1/alerts` | pending and firing alerts |
| `GET` `PUT /api/v1/alerting/config` | the Alertmanager configuration (YAML); admin only |
| `/alertmanager/api/v2/…` | Alertmanager's own API: silences, alert groups, receivers, status |

```bash
curl -X PUT -H "Authorization: Bearer $T" --data-binary @rules.yaml https://vigil/api/v1/rules/config
```

## Fleet

| | |
|---|---|
| `GET /api/v1/agents` | agents with labels, facts, profiles, integrations, what was detected, config state |
| `GET /api/v1/agents/{id}/config` | the agent's merged configuration and where each setting comes from |
| `DELETE /api/v1/agents/{id}` | forget an agent |
| `POST` `DELETE /api/v1/agents/{id}/token` | issue (returned once) or revoke its token |
| `GET` `POST /api/v1/fleet/enrollment` | enrolment tokens (admin). `POST {"name": "...", "valid_hours": 24, "max_uses": 0}` returns the token once; 0 means no limit |
| `DELETE /api/v1/fleet/enrollment/{id}` | revoke one |
| `GET /api/v1/server-info` | the server's address, version, and `ca_pin` if it has its own certificate authority |
| `GET /api/v1/fleet/profiles` | profiles with selector, priority, version, how many agents match |
| `GET` `PUT` `DELETE /api/v1/fleet/profiles/{name}` | one profile (YAML body on `PUT`; `?rollout=skip` applies it to everyone at once) |
| `GET /api/v1/fleet/profiles/{name}/versions`, `…/versions/{n}` | history |
| `GET /api/v1/fleet/rollouts` | rollouts in progress: step, agents and their state, why one is paused |
| `POST /api/v1/fleet/rollouts/{profile}/{action}` | `promote`, `complete`, `pause`, `resume` or `abort` |
| `POST /api/v1/fleet/match` | preview: `{"name": "...", "doc": "<profile yaml>"}` → which agents match, and whether saving would be refused |
| `GET /api/v1/fleet/integrations` | the catalog with detection counts |
| `GET` `PUT` `DELETE /api/v1/fleet/integrations/{name}` | one integration |

`vigil apply -f DIR` uses these.

## Dashboards, captures, users

| | |
|---|---|
| `GET /api/v1/dashboards` | list |
| `GET` `PUT` `DELETE /api/v1/dashboards/{id}` | one dashboard (JSON) |
| `GET /api/v1/dashboards/{id}/update` | the newer version on offer for a dashboard that comes with vigil; 204 if none |
| `POST /api/v1/dashboards/{id}/update` | `{"action": "apply"}` (with `"copy": true` to keep the current one as `<id>-mine`), `"keep"` or `"restore"` |
| `GET /api/v1/captures` | list, newest first |
| `GET` `DELETE /api/v1/captures/{id}` | one capture (JSON) |
| `GET /api/v1/users` | accounts (admin) |
| `PUT` `DELETE /api/v1/users/{name}` | create or change (`{"password": "...", "role": "admin"}`), delete |
| `POST /api/v1/login`, `/logout`; `GET /api/v1/me`; `POST /api/v1/me/password` | sessions. Signing in needs the header `X-Vigil-CSRF: 1`, like every changing request made with a session |
| `GET /healthz` | `ok`, without signing in |

## What agents send

For completeness; you do not normally call these yourself.

| | |
|---|---|
| `POST /api/v1/write` | metrics, Prometheus remote-write |
| `POST /api/v1/logs` | log lines |
| `POST /api/v1/events` | security detections |
| `POST /api/v1/captures` | a capture |
| `POST /api/v1/traces` | spans, as a gzip-compressed OTLP trace request |
| `POST /api/v1/agent/config` | the agent says who it is and gets its configuration |
| `POST /api/v1/enroll` | `{"token": "vge_…", "agent_id": "..."}` → `{"token": "vgl_…"}`. Needs no other credential |
| `GET /api/v1/ca` | the certificate of the server's own authority (PEM), public |

All but the last two need an agent token and the header `X-Vigil-Agent: <id>`. The server
stamps everything with that id, so an agent cannot write as another.

`429` means the agent is over the server's [limits](install.md#limits) and
`503` that the server or a store is busy or away; in both cases the batch
was not stored and should be sent again.

A [relay](relay.md) accepts exactly these requests and passes them on,
adding `X-Vigil-Relay: <its id>`.

Because `/api/v1/write` speaks Prometheus remote-write, a Prometheus or
anything else that can remote-write can push into vigil with an agent token.

## Grafana

Add a Prometheus data source pointing at the server's address, with the
header `Authorization: Bearer <admin token>`.
