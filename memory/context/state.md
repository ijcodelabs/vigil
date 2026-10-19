# State

Last updated: 2026-10-13.

## Built (steps 1–6 of the original plan)

1. **Agent and server.** One static Go binary, `vigil agent` / `vigil server`.
   Host metrics, Prometheus scraping (static lists, Kubernetes pod
   annotations), push to the server through a disk spool, heartbeats,
   Prometheus query API.
2. **Logs.** Files, journald, Docker, Kubernetes pod logs; parsing templates;
   positions survive restarts. Docker container metrics.
3. **Rules and alerting.** Prometheus-format rule file, with `type: logs`
   groups for LogsQL. Alertmanager v0.34.1 embedded for routing, silences and
   notifications.
4. **Web UI and fleet.** Explore, Alerts, Fleet, Users. Sign-in with admin and
   viewer roles. Per-agent tokens. Server-managed agent config.
5. **Runtime events.** eBPF probes (exec, exit, connect, listen, open, setuid),
   CEL rules evaluated on the agent, flight-recorder captures, Captures page.
6. **Dashboards.** Time-series, stat, table and log panels; variables; editor;
   three starter dashboards.

## Config at scale, step 1 of 4: profiles (built)

- Agents report labels (from their file) and facts (os, kernel, arch, ...).
- Profiles: selector, priority and a config fragment, merged on the server
  into each agent's config (`internal/fleet`). Scrape jobs, log sources and
  security rules merge by name; `enabled: false` switches one off.
- A save is refused if any known agent would get an invalid config.
- Effective-config view with the origin of every setting; versions with
  compare and restore; `vigil apply -f DIR` for profiles kept in git.
- Per-job scrape settings: interval, timeout, auth, keep/drop.
- Old managed configs (one default, whole-document overrides per agent) are
  migrated to profiles `default` and `agent-<id>` on first start.

## Config at scale, step 2 of 4: integrations (built)

- An integration bundles detection, a config fragment with parameters, rule
  groups and a dashboard (`internal/fleet/integration.go`, built-ins in
  `internal/fleet/integrations/`). Profiles switch them on.
- Agents report an inventory (process names, listening ports, container
  images); the server derives `detected_<name>` attributes from it, usable in
  selectors, and shows what runs unmonitored.
- Rules of integrations in use run next to the rule file; dashboards are
  installed once.
- Only the VictoriaMetrics and Docker integrations have run against the real
  thing. nginx, PostgreSQL and Redis are written from their exporters'
  documented metric names; their log templates are tested on sample lines.

## Config at scale, step 3 of 4: new sources and limits (built)

- Probes (HTTP, TCP, certificate expiry), process groups, systemd unit
  states, more host metrics (pressure, file descriptors, retransmits, OOM
  kills, read-only filesystems), agent self-metrics.
- An OTLP endpoint in the agent (HTTP, protobuf) for metrics and logs:
  step 1 of the OpenTelemetry plan in direction.md. Traces are accepted and
  dropped. No gRPC, no JSON encoding.
- Per-agent limits on series and log bytes, enforced on the agent.
- All of it ran on a real Linux host with systemd and journald (the Podman
  VM itself), with OpenTelemetry's telemetrygen as the OTLP sender.
- Not built: per-container metrics from the kubelet. Nothing Kubernetes has
  run against a real cluster yet; that needs its own pass with one.

## Kubernetes pass (built, on a real cluster)

- Ran against k3s v1.34 (containerd) inside the Podman VM; see testing.md.
- Verified for real: the DaemonSet manifest, pod discovery by annotation,
  node host metrics, pod logs, facts, detection, and eBPF events inside pods.
- Added: per-container metrics from the kubelet and pod state from the API
  (`internal/kube`), log templates and opt-out by pod annotation, the `app`
  label on pod logs, pod names on runtime events, a `kubernetes` integration
  with rules and a dashboard.
- Found and fixed: the DaemonSet did not start (mountPropagation on the host
  root mount needs a shared mount, which nested clusters do not have).
- Cluster-level state is a second agent per cluster
  (`deploy/k8s-cluster-agent.yaml`, `cluster.enabled`, `internal/kube/cluster.go`):
  nodes, workloads, jobs, volume claims, unschedulable pods, and events as
  logs. Agents report `kubernetes_role` node or cluster; integration
  `kubernetes_cluster`. It lists objects on an interval (no watches).

## Config at scale, step 4 of 4, first part: enrolment and encryption (built)

- `tls.self_signed`: the server has its own certificate authority
  (`internal/pki`, files in `data_dir/pki`), serves with a leaf it renews
  itself, and adds whatever name or address a client used to that leaf.
  Agents trust it by fingerprint (`server.ca_pin`): they fetch `/api/v1/ca`
  once, check the fingerprint, cache it in the spool. `tls.cert_file` /
  `key_file` for a supplied certificate; `server.ca_file` on agents.
- Enrolment tokens (`vge_…`, Fleet > Enrolment, table `enroll_tokens`, hash
  only, expiry and use limit). `POST /api/v1/enroll` gives a per-agent
  token; an agent id that already has a token is refused. Agents either run
  `vigil enroll` (writes agent.yaml) or carry `server.enroll_token` and
  enrol on first start (`internal/agent/connect.go`, token kept in
  `spool_dir/agent-token`). Nothing is sent before that step finishes.
- Verified live: test server on TLS, laptop agent and the k3s DaemonSet
  enrolled by token + pin, cluster agent with a hand-made token.
- Not built: client certificates (agents authenticate by token only),
  rotation of agent tokens, reloading a supplied certificate without a
  restart. The cluster agent cannot self-enrol (no persistent disk).
- The built-in browser pane refuses the test server's certificate; for UI
  checks a plain-HTTP proxy runs on 127.0.0.1:18081 (see testing.md).

## Config at scale, step 4 of 4, second part: staged rollout (built)

- A profile with `rollout: {steps: [1, 25%], wait: 10m}` spreads each change
  in steps (`internal/fleet/rollout.go`, `internal/server/rollout.go`, table
  `rollouts`). While it runs the server keeps both versions: admitted agents
  resolve with the new one, the rest with the one from before
  (`fleetState.profilesFor`). Steps count only agents whose merged config
  really differs.
- Pauses by itself when an admitted agent reports it cannot apply the change
  or goes silent. Promote / complete / pause / resume / abort from the
  Profiles tab or the API. Abort saves the earlier text as a new version.
- Health is only "applied and still reporting". Not built: looking at
  alerts or metrics of the canaries, staged deletes, staged changes to
  integrations (their documents apply at once).

## Trial on a real server (running since 2026-10-10)

- vigil runs on a production Docker host (x86-64, Debian 13, kernel 6.12)
  next to the monitoring that host already had. What is deployed there, and
  how, is in the private `vigil-deployments` repository with its own state
  notes; nothing that names that deployment belongs in this repository.
- First numbers: 7 of 7 scrape targets up, about 5,500 series, agent 3% of
  one core and 126 MiB, nothing dropped.
- `security` (eBPF) ran there for 45 minutes on 2026-10-10 and is off again:
  about 28 events/s, none lost, agent CPU 5% -> 8% of one core (peak 13%),
  host CPU unchanged, no restarts. 261 of 268 detections were the postgres
  health check reading the container's own /etc/shadow: a path in an event
  is as the process sees it, so rules about host files need
  `container == ""`. Captures were rate-limited as designed (12 for 268
  detections).
- Reached by people through an HAProxy edge since 2026-10-11, with
  `trusted_proxies` set. The laptop's rsync is openrsync (no `--chmod`), so
  deploy scripts set permissions on the server.
- Seen there and missing in vigil: Postgres and Redis integrations need
  exporters that host does not run.

## Config at scale, step 4 of 4, third part: relay mode (built)

- `vigil relay -config relay.yaml` (`internal/relay`): a stateless reverse
  proxy for the six agent endpoints only (write, logs, events, captures,
  agent/config, enroll). The server still checks every token; the relay adds
  `X-Vigil-Relay`, which the server stores as the agent's `via` (display
  only). Relays can be chained. Own listening certificate via
  `tls.self_signed` (own CA, agents pin the relay's fingerprint) or files.
- No buffering in the relay by design: on failure agents get 503 and keep
  their spools. `max_in_flight` bounds concurrency. `/metrics` on the relay.
- Not built: the relay authenticating itself to the server, per-agent
  addresses behind a relay (the server sees the relay's address, also for
  the enrolment rate limit), spooling in the relay.

## Config at scale, step 4 of 4, last part: server-side limits (built)

- `limits` in the server config (`internal/server/limits.go`): per-agent
  samples/s and log bytes/s as token buckets over a minute (429, agent
  retries and eventually drops its own oldest data), `max_in_flight` for all
  ingest (503), per-agent overrides. Defaults apply only to a config loaded
  from a file; zero in code means no limit.
- Seen in the Fleet list (`limited`), and as
  `vigil_agent_limited_batches_total`, written with the heartbeats only for
  agents it happened to.
- On the same path: agent tokens are cached in memory (dropped on any token
  change), the agents table is written at most every 5 s per agent, and the
  server waits up to 30 s for the metric store at start.
- Not limited: detections and captures per agent (captures have a size cap
  and a disk budget), number of agents, query load from the UI.

## From the trial (built)

- `docker.container_templates`: a log template by container or image name
  (globs), after the container's own label and before the default.
- Log templates can `add` fixed stream labels.
- The HAProxy integration parses `option httplog` lines (template
  `haproxy_http`, lines get `app="haproxy"`), with a log rule and three log
  panels. Integration parameters may now be used in map keys.
- The agent reports its own cost: `vigil_agent_cpu_seconds_total`,
  `vigil_agent_memory_bytes`, open files, goroutines, start time. A fourth
  starter dashboard, "vigil agents", shows them. Starter dashboards are now
  seeded one by one (setting `dashboards_seeded` lists the ids), so a new
  one reaches servers that already exist; a deleted one does not come back.
- Dashboards that come with vigil (starters, integration dashboards) are
  tracked by a fingerprint of what was installed (`internal/server/provided.go`,
  settings `dashboard_provided:<id>`): untouched ones follow new versions,
  changed ones get the new version offered (notice with update / update and
  keep a copy / keep mine), deleted ones stay deleted (`restore` via API).
  Installs from before this count as changed unless identical.
- journald: lines get `unit` from the system unit, else the user unit, else
  `kernel`/`audit` by transport; `transport` is a field; byte-array messages
  are decoded. `logs.journald.unit_templates` picks a template per unit, and
  multi-line joining is per unit (its own pump in `journald.go`; a late
  joined entry carries the newest cursor so the position never goes back).
- Version skew to keep in mind: a server newer than an agent can send it
  config fields it does not know; the agent refuses that config, keeps the
  old one and reports the error. Update agents before the server when an
  integration starts using a new field.

## OpenTelemetry as APM (built 2026-10-11)

- Agent (`internal/otlp/traces.go`): every span is counted into
  `traces_spans_total`, `traces_span_duration_seconds_*` (entry and exit
  spans, by service/operation/kind/status, at most 200 operation names per
  service) and `traces_service_calls_total` (who calls what, from client
  span attributes), sent every 15 s. Sampling by
  trace id (`otlp.traces.sample`), failed and slow spans always kept.
- Trace store: VictoriaTraces (user's choice, 2026-10-11; v0.12.0 tried),
  `storage.traces_url`, optional. Agents ship kept spans as gzip OTLP to
  `POST /api/v1/traces` (own spool, shares the log byte allowance); the
  server forwards to `/insert/opentelemetry/v1/traces` with
  `VT-Extra-Fields: agent=<id>` and proxies the store's Jaeger API and
  LogsQL under `/api/v1/traces/…` (`internal/server/traces.go`). The first
  version kept spans in VictoriaLogs; that path is gone.
- UI (`services.js`): Services list, service page (charts, operations,
  dependencies, stored traces with filters, from LogsQL over spans), trace
  waterfall (from the Jaeger API); log lines link to their trace.
- `vigil tracegen` sends demo traces of a small shop (`internal/otlp/demo.go`).
- Not built: gRPC OTLP, a drawn service map, tail-based sampling, exemplars,
  eBPF auto-instrumentation.

## GPUs and AI costs (built 2026-10-11)

- `internal/gpu`: NVIDIA through `nvidia-smi` CSV queries (per GPU and per
  process, with user and container), AMD through sysfs. On by itself where
  a GPU can be read; fact `gpu`; integration `gpu` (rules, dashboard).
  NEVER RUN ON REAL HARDWARE: tested against output in nvidia-smi's format
  and a made-up sysfs tree only.
- Tokens: spans following OpenTelemetry's GenAI conventions are counted by
  the agent into `genai_requests_total` and `genai_tokens_total`
  (`internal/otlp/traces.go`, several attribute spellings accepted).
- Prices: `costs` in the server config (tokens per million by model-name
  prefix, GPU hours by card model). `internal/server/costs.go` writes
  `genai_token_price` and `gpu_hour_price` for the models actually in use,
  so cost is a plain join. No built-in prices, on purpose.
- Starter dashboard "AI costs"; dashboards understand `$__range` and
  `$__range_s`.
- Not built: reading the providers' usage and billing APIs (OpenAI,
  Anthropic, Google) to reconcile with the invoice; per-pod GPU allocation
  on Kubernetes; Slurm.
- Telegram is the receiver on the trial (token in a file on the server).

## Trial, traces (2026-10-11)

- VictoriaTraces runs next to the server. A second agent runs as a
  container on the applications' Docker network as an OTLP gateway for the
  other containers. One replica of a real web API is traced through it: the
  first real application in the Services pages; server spans, Redis calls
  and SQL arrive.
- Dashboards ran into the edge's shared limit of 50 requests / 10 s per
  address; vigil got its own (600).
- On hold by the user's decision: GPU monitoring on real hardware, and a
  local model for step 7, until a GPU machine exists.

## Agents outside the server's network (2026-10-11)

- A NixOS host (unit from `deploy/vigil-agent.nix`) and an Arch host
  (ordinary unit, capped at half a core and 256 MB) report through the
  public name, so the edge has to let the agent paths through. Installed by
  following `docs/install-by-ai.md`, which gained a NixOS section and a
  proxy check from it.
- Upgrading those agents: copy the new binary and restart the unit by hand;
  nothing redeploys them.

## Down alerts and built-in rules (built 2026-10-12)

- A rule group `builtin` runs on every server (`internal/server/builtin_rules.yaml`,
  `GET /api/v1/rules/builtin`): HostDown, AgentDroppingData, ProbeFailing,
  CertificateExpiresSoon. An alert of the same name in the rule file
  replaces the built-in one; a group named `builtin` there replaces all.
- The Docker collector reports every container, not only running ones:
  `container_running`, `container_exit_code` (while stopped),
  `container_restarts_total`. One more API call per container and interval.
  The Docker integration gained ContainerDown (stopped with a non-zero exit
  code for two minutes) and ContainerRestarting.
- Verified live against Podman's Docker API and against Docker 29.9 on
  Ubuntu 24.04 (agent and server as systemd units; HostDown fired there): exit 3 and a kill (137) alert,
  exit 0 does not, a restarted container clears within about two minutes, a
  restart loop is caught, HostDown goes pending for a stopped agent.
- Known: `container_exit_code` stays visible for a few minutes after a
  container starts again (the store's lookback), which is why the rule also
  asks for `container_running == 0`. Container alerts of a host that went
  down linger for that long too.
- Not built: opting a container out by label; `vigil apply` for the rule
  file (it is uploaded with curl); pod and node rules were already there.

## Site checks (built 2026-10-12)

- Probes gained `tls:` (handshake and certificate of anything that starts
  with TLS), `ping:` (three ICMP echo requests; an unprivileged ping socket
  first, a raw socket second) and `expect_body` for HTTP
  (`internal/probe`). Every probe metric now carries `type` and `target`
  (a URL without user, password and query).
- The certificate is verified by the probe itself, not by the handshake, so
  the time left is reported also for a refused certificate; an expired one
  is negative. Built-in rule `CertificateExpired` next to
  `CertificateExpiresSoon`.
- Starter dashboard "Site checks" (`checks`): one table with a row per
  check (answers, HTTP status, time in this state, time to answer, share
  answered and failures in the range, certificate left), where the time
  goes, slowest checks, filters by agent, type and check.
- HTTP probes report where the time went (`probe_dns_seconds`,
  `probe_connect_seconds`, `probe_tls_handshake_seconds`,
  `probe_first_byte_seconds`), and the certificate of the address they
  name, not of where it redirects to.
- Table panels take several queries as columns, matched by labels, each
  with its own unit and yellow/red thresholds (red below yellow: low is
  bad); `columns` picks the label columns. Unit `yesno`.
- The Containers dashboard lists containers that are not running, with exit
  code, time stopped and restarts.
- ContainerRestarting (Docker and Kubernetes) compares with the value 15 or
  30 minutes ago: the store's increase() counts a counter it first sees as
  risen from zero, which fired for every container on the trial host.
- "In this state for" lists only checks that report now; before
  2026-10-12 a check that had been removed stayed as a row for 30 days.
- "In this state for" and "Failures in range" use MetricsQL
  (`tlast_change_over_time`, `decreases_over_time`), so those dashboards
  need VictoriaMetrics, not any Prometheus-compatible store.
- Verified live on Ubuntu 24.04 as root: all four types, a wrong body, a
  closed port, an expired public certificate (negative days,
  CertificateExpired fired), ping to an address nobody has.
- Known: changing a probe's target makes a new series, and the old one
  lingers for the store's lookback (a few minutes) with its last value.
  Lima's network answers echo requests for every address beyond the
  machine, so "nobody answers" needs an unused address on a local bridge.
- Not built: checks run by the server itself (no agent), STARTTLS, DNS
  checks, timing per phase (DNS, connect, TLS), ping as a non-root user on
  a host that does not allow ping sockets (it fails, as documented), IPv6
  ping against a real host.

## Silences are announced (built 2026-10-12)

- `internal/alerting/silences.go`: a new silence is reported to the
  receivers of the alerts it covers (or, covering none, of its own labels)
  as an alert `AlertsSilenced`; an alert that ends while silenced is
  reported as resolved to the receivers that had been told it fires. Both
  go straight to the receivers' integrations, past grouping and silences.
- "Had been told" is recorded by wrapping every integration; in memory
  only, so it is lost with a restart.
- Silences go through `POST /alertmanager/api/v2/silences` in the server
  (`postSilence`), which signs them with the signed-in account.
  `notify_silences: false` switches the messages off.
- Sent through a real Telegram receiver on 2026-10-12: delivered, with an
  error line in the server log about a missing "notification reason",
  which the messages now carry. Slack and mail receivers: never tried.
- A message template can tell the silence message apart by
  `.GroupLabels.alertname` being `AlertsSilenced`.
- Explore > Logs: "exclude" next to "filter" on a line's fields.

## Choosing what an agent sends: container logs (built 2026-10-12)

- `docker.container_logs`: a map of container or image name (with * and ?)
  to true or false; the most specific key wins, the container's
  `vigil.logs` label overrules it. A map, so profiles add entries.
- Explore > Log volume: lines and bytes per source from the log store
  (`stats by (...) count(), sum_len(_msg)`), with "stop collecting" for a
  container. That calls `POST /api/v1/agents/{id}/container-logs`
  (`internal/server/containerlogs.go`), which edits the profile
  `agent-<id>` as YAML nodes, keeping the rest of the document, and saves it
  through the normal profile save.
- Verified live on Docker: a noisy container stopped arriving 12 s after
  the click and came back 18 s after "collect again".
- `logs.drop`: named rules (source by container, unit, pod or app; a
  regular expression on the message, none meaning every line), applied in
  the agent's central log loop to the entry as it would be stored, so the
  log store can preview a rule exactly. Keyed by name across profiles.
- Explore > Logs: "Stop sending lines like this" proposes a pattern from a
  line (`suggestPattern`), previews it against the last hour of that
  source, and saves it through `POST /api/v1/agents/{id}/log-drops`.
  Log volume stops units, pods and apps with a rule without a pattern and
  lists and removes the rules.
- Verified live: a rule made from a health-check line dropped those lines
  and kept the container's error lines; a unit was switched off and a rule
  removed from the page.
- Not built: a count of what each rule drops; log files as a source of a
  rule; a minimum level per source; a rate limit per container.

## Saved queries in Explore (built 2026-10-13)

- `internal/server/queries.go`, table `saved_queries`: a query is personal
  (its owner only, viewers may have them) or shared (everyone sees it,
  admins change it). `GET/POST /api/v1/queries`, `PUT/DELETE
  /api/v1/queries/{id}`. Deleting an account deletes its personal ones.
- Queries that come with vigil are not stored: `starter_queries.yaml` in
  the server, and `queries:` in an integration (all nine built-in ones
  have some), listed while the integration is in use. Read-only.
- Shared ones can come from files: `vigil apply` reads `DIR/queries/*.yaml`
  (`fleet.ParseQueries`), origin `files`, `-prune` as for profiles.
- UI in `saved.js`: the Saved menu (search, yours / shared / comes with
  vigil / recent), Save (new, or update the one it came from), and for
  admins "Add to": a dashboard panel through the dashboard's own
  `PanelEditor`, or an alert rule.
- Recent queries are in the browser's localStorage per account, 20 each
  for metrics and logs; the server knows nothing of them.
- `POST /api/v1/rules/config/alerts` (`rulealert.go`) writes one alert into
  the rule file as YAML nodes, keeping comments, then saves through the
  same check as the whole file. It refuses the names of built-in alerts
  and the group names `builtin` and anything with a slash.
  `GET /api/v1/rules/config/groups` lists the file's groups.
- Rule manager (`internal/rules/manager.go`, `Apply`): the firing alerts
  of a rule that was removed are sent to the notifier as resolved at once
  (before, they stayed there until their four-minute timeout); a rule
  whose expression was edited keeps its alerts (same group and name, one
  gone and one new). Found by removing an alert made from Explore.
- Dashboards, edit mode: a panel is moved by dragging its title (native
  drag and drop; it takes the place before or after the panel it is
  dropped on, the layout stays a flow of panels in order) and resized by
  dragging its corner, in whole columns and rows. The new order is applied
  when the drag ends, and tile contents ignore the pointer during a move:
  charts are redrawn on every render, and a drop target that is replaced
  under the pointer loses the drop. Not built: free placement with gaps,
  dragging on touch screens.
- Verified live on the Lima host: every shipped query is accepted by its
  store; save, update, share, pick, both "Add to" paths, a viewer's view,
  and `vigil apply` with save, unchanged, change and prune.
- Not built: folders or tags for saved queries, saving the time range,
  hiding a query that comes with vigil, removing an alert again from
  Explore (edit the rule file), `vigil apply` for the rule file itself.

## Grafana on top of the server (built 2026-10-13)

- Read-only tokens (`internal/server/tokens.go`, table `api_tokens`, hash
  only, `vgr_…`, optional expiry, last use noted at most once a minute):
  a viewer that may only GET, plus POST on the query APIs (`s.asks`).
  Made and revoked on the Users page.
- The log query operations also answer under `/select/logsql/…`, so
  Grafana's VictoriaLogs data source can be given the server's address.
  `/api/v1/metadata` and `/api/v1/status/buildinfo` go to the metric store.
- `deploy/grafana/datasources.yaml` provisions four data sources
  (Prometheus, VictoriaLogs plugin, Jaeger, Alertmanager), all through
  the server with one token. `docs/grafana.md`.
- Alertmanager's `/api/v2/status` carries its whole configuration and was
  readable by viewers; non-admins now get it without the configuration.
- Verified live with Grafana 13.2 in the Lima host: health checks of the
  metrics and logs data sources, range queries, label and metric lookups,
  log lines and `| stats` over time, alerts and silences read through the
  Alertmanager data source, a silence made from Grafana refused.
- Seen: Grafana's Explore says "No logs volume available" with the
  VictoriaLogs plugin on Grafana 13.2, also when the plugin talks to the
  log store directly; the hits query itself answers through the server.
- Not tried: the Jaeger data source against a real trace store (the test
  host has none), a server with its own certificate authority, the
  VictoriaMetrics data source plugin (only the Prometheus one).
- Not built: exporting vigil's dashboards as Grafana JSON, tokens limited
  to some agents or labels, a limit on query load per token.

## AI module for Claude Code, step 2 of the plan: agent side (built 2026-10-19)

- A real Claude Code session (2.1.303, every content flag on, one Explore
  subagent, `cat .env`) was recorded through a local capture and is kept,
  scrubbed, as `internal/otlp/testdata/claude_code/` (protojson).
- What it sends: the event name as the body (no change needed), `team`
  and every resource attribute copied onto points and events, about 17
  labels per metric point including `session_id` and four account ids,
  `prompt.id` and the interaction's trace id on every event, `agent_id`
  on subagent spans, and a tool's output as a `tool.output` span event.
  With `OTEL_LOG_TOOL_CONTENT` the output of `cat .env` arrived in clear.
- `otlp.metrics.drop_labels` (a map, merged across profiles): labels left
  out of every OTLP point; delta points that then coincide are added up.
- `redact` (`internal/redact`): built-in rules for keys, tokens, private
  keys, passwords in URLs and values named password/secret/token/api_key,
  on unless `builtin: false`; `rules` merge by name. Applied on the agent
  to every log line from every source (message and fields, not stream
  labels) and to the attributes and events of stored spans.
- Spans of `claude-code` / `claude-code-desktop` are not counted into
  `genai_*`: Claude Code reports its usage itself, and its spans carry
  model names without `gen_ai.usage.*` tokens.
- Verified live: the masked `.env` line in VictoriaTraces, no session or
  account labels on `claude_code_token_usage_total`, no new `genai_*`
  samples from Claude Code.
- Not yet: the `ai_module_claude` integration, the Sessions page, hooks.
  Sessions started by the desktop app did not export at all in the
  capture (terminal `claude` did); not understood yet.

## Next

- The config-at-scale list is done. Open: step 7 (AI investigator, MCP
  server), and the watched eBPF window on the trial host.
- Step 7 of the original plan: AI investigator and MCP server. Open question:
  Claude API, or must it run with local models?

## Never run for real

- Any kernel older than 6.12; there is no fallback below 5.8. (The x86-64
  probe loaded and worked on Debian 13, kernel 6.12, on 2026-10-10: all five
  event types, container names, about 33 events a second on a busy Docker
  host, no measurable change in host CPU.)
- Kubernetes on a multi-node cluster, or with a runtime other than containerd.
