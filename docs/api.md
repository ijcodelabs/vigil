# API

Everything the interface does goes through this API, on the server's
address.

## Signing requests

```bash
curl -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://vigil.example.com/api/v1/agents
```

- The **admin token** may do everything.
- A **read-only token** (Users → Read-only tokens, `vgr_…`) may read what
  a viewer can and change nothing. It is the one to give a script that
  only pulls numbers, or [Grafana](grafana.md). `GET` `POST /api/v1/tokens`
  and `DELETE /api/v1/tokens/{id}` manage them (admins); a new token's
  value is in the answer to the `POST` and nowhere afterwards.
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
| `GET` `POST /select/logsql/…` | the same log operations under the log store's own paths, for tools that expect a VictoriaLogs |
| `GET /api/v1/metadata`, `/api/v1/status/buildinfo` | asked by tools that speak Prometheus' API |

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
| `GET /api/v1/briefing` | what the Home page shows: open incidents (those of a host that is down nested under it), incidents resolved in the last 24 hours, what happened since the caller last marked the briefing seen, and coverage (agents reporting, rules failing, silences in effect) |
| `POST /api/v1/briefing/seen` | `{"event_id": N}`: the caller has seen everything up to that event (`since.latest` of the briefing they were shown); never moves back |
| `GET /api/v1/incidents` | resolved incidents, newest first; `?days=7` (at most 30), `?limit=100` (at most 500) |
| `GET /api/v1/incidents/{id}` | one incident and its history (opened, resolved, reopened, acknowledged, silenced) |
| `POST /api/v1/incidents/{id}/ack` | `{"comment": "…"}`: someone is on it; admin only. Lasts until the incident is resolved |
| `GET /api/v1/ai` | whether AI explanations are set up and paused, the model, what may be sent, the monthly budget and what was spent |
| `PUT /api/v1/ai/paused` | `{"paused": true}`: stop sending anything; admin only |
| `GET /api/v1/incidents/{id}/explain` | the evidence that would be sent, without sending it; admin only |
| `POST /api/v1/incidents/{id}/explain` | send it and return the explanation; admin only. 409 when not set up, paused or over budget |
| `GET /api/v1/incidents/{id}/explanations` | the last ten explanations of an incident, with the evidence as sent |
| `POST /api/v1/explanations/{id}/verdict` | `{"verdict": "right" \| "partly" \| "wrong" \| "unhelpful", "note": "…"}` |
| `POST /api/v1/loglines/context` | `{"line": {…}}`, a line as the log query answered it (needs `_time`, `agent`, `_stream_id`): lines like it, the lines around it, what happened on the host, open incidents, notes |
| `POST /api/v1/loglines/explain/preview` | the same body: the evidence that would be sent, and an earlier explanation of lines like it if there is one; admin only |
| `POST /api/v1/loglines/explain` | send it and return the explanation (`"again": true` for a new one instead of the earlier); admin only. 409 without `data: [logs]` |

```bash
curl -X PUT -H "Authorization: Bearer $T" --data-binary @rules.yaml https://vigil/api/v1/rules/config
```

## Service level objectives

| | |
|---|---|
| `GET /api/v1/slos` | every SLO: the parsed document, its kind, version, origin and author. With `?status=1`, `{"slos": […], "status": […]}`: per SLO the window (`from`, `to`), `good`, `total`, `achieved` (percent), `budget_left` (1 untouched, below 0 overspent), `burn` by window (`1h`, `6h`, `1d`, `3d`), `counting`, `coverage` and the alerts `firing`; 502 when the metric store does not answer |
| `GET /api/v1/slos/{name}` | one SLO with its document (`doc`), the rule groups made of it, its `status` (as above), its `incidents` in the window and, for span SLOs, the `operations` queries |
| `PUT /api/v1/slos/{name}` | save one (YAML body); admin only. Refused (400) if it does not parse or the metric store refuses its queries; the answer has `version`, `changed` (false for an identical document) and `warnings` |
| `DELETE /api/v1/slos/{name}` | delete one; admin only. Its recorded series stay until the store's retention ends |
| `GET /api/v1/slos/{name}/versions`, `…/versions/{n}` | history |
| `GET /api/v1/slos/report?period=2026-10` | the SLA report of a month (`2026-10`) or quarter (`2026-Q4`), last month without `period`; `&format=csv` for CSV. `final` is false while the period runs; a finished period's report is stored and does not change |
| `GET /api/v1/slos/reports` | the stored reports: `period`, `created` |
| `GET /api/v1/slos/templates` | admin only: SLOs the templates offer for what reported in the last day: `template`, `title`, `value`, `name`, the filled-in `doc`, and `exists` |

See [Service level objectives](slo.md).

## Fleet

| | |
|---|---|
| `GET /api/v1/agents` | agents with labels, facts, profiles, integrations, what was detected, config state |
| `GET /api/v1/agents/{id}/config` | the agent's merged configuration and where each setting comes from |
| `DELETE /api/v1/agents/{id}` | forget an agent |
| `POST` `DELETE /api/v1/agents/{id}/token` | issue (returned once) or revoke its token |
| `POST /api/v1/agents/{id}/container-logs` | `{"container": "name or pattern", "collect": false}`: stop (or, with `true`, resume) collecting a container's logs on that agent, by editing the profile `agent-<id>` |
| `GET /api/v1/fleet/container-logs` | every `docker.container_logs` entry of every profile |
| `POST /api/v1/agents/{id}/log-drops` | `{"unit": "name", "match": "regex"}` (or `container`, `pod`, `app`; an empty `match` is every line): the agent stops sending those lines; written to the profile `agent-<id>` |
| `DELETE /api/v1/agents/{id}/log-drops/{name}` | remove such a rule |
| `GET /api/v1/fleet/log-drops` | every `logs.drop` rule of every profile |
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
| `POST /api/v1/login`, `/logout`; `GET /api/v1/me`; `POST /api/v1/me/password` | sessions. `/me` answers with who you are, your role and the server's `version`. Signing in needs the header `X-Vigil-CSRF: 1`, like every changing request made with a session |
| `GET /healthz` | `ok`, without signing in |
| `GET /api/v1/secrets` | the secret store (never its secret_id or token), its health, and where each secret vigil uses comes from, with whether it is there (never the value); admin only |
| `POST /api/v1/secrets/store/test` | `{"store": {…}, "credential": "…"}`: log in and report what the login may do, without saving |
| `PUT /api/v1/secrets/store` | the same plus `"password"` (yours, or the admin token): test and save. An empty credential keeps the saved one only for the same address and login |
| `POST /api/v1/secrets/store/remove` | `{"password": "…"}` |
| `GET /api/v1/secrets/store/list?prefix=` | secret names under the mount |
| `PUT /api/v1/secrets/uses/{name}` | `{"path": "anthropic", "field": "api_key"}`; `name` is `ai_api_key` |
| `POST /api/v1/secrets/uses/{name}/test` | read it again, and for the AI key ask the provider whether it accepts it |

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
