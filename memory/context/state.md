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
  Since 2026-10-30 the same receiver also posts to a channel in Boop Chat
  through `slack_configs`: Boop Chat takes Slack's incoming-webhook format,
  so no vigil code was needed, only `docs/alerts.md` ("Boop Chat,
  Mattermost and other Slack-compatible chats"). Payloads from the bundled
  Alertmanager (v0.34) were replayed into Boop Chat's webhook handler and
  accepted; it refuses unknown attachment fields, so recheck after an
  Alertmanager upgrade.

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
- Why desktop-app sessions did not export (found 2026-10-19, Claude Code
  2.1.299 and 2.1.303): telemetry `env` from a project's
  `.claude/settings.local.json` is obeyed only by interactive terminal
  sessions; `claude -p` and the desktop app (which runs Claude Code
  without a terminal, `--setting-sources=user,project,local`) ignore it,
  also in a trusted folder. The same env in `~/.claude/settings.json` or
  `--settings` exports from both. The desktop app sets
  `OTEL_SERVICE_NAME=claude-code-desktop` and its own
  `OTEL_RESOURCE_ATTRIBUTES`, which wins over one from settings, so
  `team` has to come from the agent's labels there. Confirmed with real
  desktop sessions after putting the env in `~/.claude/settings.json`
  and restarting the app: events and metrics arrive as
  `claude-code-desktop`. Seen there besides the known events:
  `hook_execution_start`/`_complete` and `retention_sweep`.

## AI module for Claude Code, step 3: the integration (built 2026-10-19)

- `ai_module_claude` (`internal/fleet/integrations/ai_module_claude.yaml`,
  example profile `configs/profiles/ai-module-claude.yaml`,
  `docs/claude-code.md`): detects the `claude` process, opens OTLP on
  the agent and drops the session and account labels; rules
  ClaudeCodeOverBudget (metrics), ClaudeCodeRunawaySession and
  ClaudeCodeApiErrors (logs); dashboard "Claude Code"; six saved queries.
- Found on the way and fixed:
  - Integration parameters never reached rules: `${...}` stayed in the
    expressions, so GpuHot and GpuIdle could never have worked. Rules are
    filled now, with the parameters of the highest-priority profile that
    switches the integration on (rules run once per server).
  - A sum sent as differences was written once per report, so the store
    had single samples and `rate()` was wrong (1.5 USD/h for 0.007 USD).
    The agent now writes a 0 when such a counter first appears and repeats
    its running total every 15 s for an hour after the last report.
  - Dashboard tables kept one result of a LogsQL query with several
    (`count() as calls, avg(ms) as avg_ms`); each now gets a column. This
    also fixes the HAProxy "slowest paths" table.
- Verified live on the Mac (agent and server native, stores in the
  vigil-test VM): detection, every dashboard and saved query accepted by
  its store, all three rules healthy, ClaudeCodeOverBudget fired with a
  profile's `daily_budget`, spend from metrics equal to the sum of the
  events' `cost_usd`, the dashboard checked in the browser.

## AI module for Claude Code, step 4: the Sessions page (built 2026-10-19)

- `internal/ui/static/js/sessions.js`, `#/sessions`: in the menu while an
  integration named `ai_module_*` is switched on. The list is LogsQL
  stats over `claude_code.api_request` (plus prompt and error counts) by
  session; a session's page reads its events (without the startup noise:
  plugin, MCP, settings, raw body and hook events) and groups them by
  `prompt_id`, with cost by part (main, each subagent, helper calls) and
  a line per event when a prompt is opened. A prompt links to its trace
  (the existing waterfall). No new server endpoint.
- Found and fixed: an OTLP log attribute with the name of a stream label
  was lost (Claude Code's `tool_decision` says who decided in `source`,
  which `source=otlp` overwrote). The agent keeps such an attribute as
  `attr_<name>`; the dashboard's "Refused tool calls" uses `attr_source`.
- Verified in the browser against the local capture (nine sessions,
  desktop and terminal): list, a session with an Explore subagent, its
  event lines, the trace link.
- Not built: colouring the trace by `agent_id`, searching sessions by
  prompt text, a session's tokens over time.

## AI module for Claude Code, step 5: audit trail from hooks (built 2026-10-19)

- `vigil claude-hook` (`cmd/vigil/claudehook.go`, `internal/claudehook`):
  a Claude Code command hook that sends one OTLP log line per call to the
  local agent (`service="claude-code-hook"`, `claude_code.hook.<Hook>`):
  session, prompt, cwd, OS account, subagent, tool and its command /
  file / pattern, sizes, errors, reasons. Tool output is never sent;
  prompt text and subagent instructions only with `-content` (telemetry
  cuts the instructions off, the hook has them whole). Prints nothing,
  always exits 0 (hook output reaches the model for some events, exit 2
  blocks a prompt), 500 ms timeout; about 80 ms a call.
- The fixture is every hook call of one real run, scrubbed
  (`internal/claudehook/testdata/hooks.jsonl`).
- The dashboard gained three audit panels and a saved query; the
  Sessions page shows hook lines in time order with Claude Code's events,
  takes a prompt's text from the hook when telemetry did not send it, and
  shows a length where Claude Code sent `<REDACTED>`.
- Verified live with `claude -p --settings` registering the hook on nine
  events: all 12 calls stored, the `.env` value absent, the session page
  showing who ran what where.
- Not built: a way to tell that hooks were removed from a machine (the
  absence of lines is the only sign); PreToolUse (it would sit in the
  path of every call).

## All AI costs in one metric (built 2026-10-24, uncommitted)

- `ai_cost_usd_per_minute{source, billing, owner, model, agent, tool}`:
  one sample a minute, each the money spent in that minute, so a range is
  `sum_over_time`. Written by recording rules: the server's `ai_costs`
  group (`internal/server/cost_rules.yaml`, applications from `genai_*`
  and `costs.tokens`, only while that list has entries), a `costs` group
  in `ai_module_claude` and in `gpu`. Anyone can add a source with a rule
  of their own (docs/finops.md).
- Counters go in as `increase(...[1m])`. A first version used
  `rate(...[5m]) * 60` and counted short-lived series up to twice on the
  trial (0.0164 against 0.0083 USD); `increase[1m]` summed minute by minute
  matched `increase` over the range exactly.
- `ai_module_claude` has a `billing` parameter, `api` (default) or
  `subscription`; subscription use is shown apart, as API-equivalent.
- The AI costs dashboard has an "All AI" section on top (billed, per day,
  subscriptions at API prices, unpriced count, cost per hour by source,
  by who and by model); the application and GPU panels stay below.
- Also fixed: the profile dialog hung when a profile reached no agents
  (`/api/v1/fleet/match` sent `agents: null`).
- Verified on the trial with real Claude Code data. Not verified: the
  application and GPU rules against real priced data (the trial has no
  `costs` prices and no GPU).

## Incidents and the briefing index (built 2026-10-24, uncommitted)

Step 1 of the Home page plan in direction.md.

- `internal/incidents`: every firing alert in the embedded Alertmanager
  (rule alerts and security detections both end up there) is a signal;
  `alerting.Alertmanager.Active` lists them with whether a silence covers
  each, plus the number of silences in effect. Key: kind, resource and
  alertname plus a hash of all labels, so two alerts that differ in any
  label are two incidents. Kinds: `host_down` (alertname HostDown),
  `security` (label source=security), `alert`. Resource from labels:
  container, probe, pod, agent, service, in that order.
- Lifecycle in SQLite (`incidents`, `incident_events`, `briefing_seen`):
  resolved one minute after the signal is gone, reopened as the same
  incident within 30 minutes, acknowledged by an admin with a comment
  until resolved. A failed read of the alerts resolves nothing. A
  `host_down` incident is the parent of the same agent's other open
  incidents. Resolved incidents are pruned after 30 days. For 3 minutes
  after the server starts nothing is resolved: Alertmanager keeps no
  alerts across a restart and rules resend theirs only when they next run,
  so before this every deploy resolved and reopened all incidents (seen on
  the trial). A rule group with an interval above 3 minutes would still
  flap once per restart.
- `internal/server/briefing.go`: a pass every 15 s; `GET /api/v1/briefing`
  (open with children, resolved in 24 h, `since` from the caller's marker,
  coverage: agents reporting, rules failing, silences, the last pass),
  `POST /api/v1/briefing/seen`, `GET /api/v1/incidents[/{id}]`,
  `POST /api/v1/incidents/{id}/ack`.
- Also a parent resource: a HAProxy server (`backend:<agent>/<proxy>/<server>`)
  hangs under its backend's incident; nesting goes to any depth, a down
  host still comes first.

## Home page (built 2026-10-24, uncommitted)

Step 2 of the plan. `#/home` is the first page (`home.js`), docs/home.md.

- Greeting, "since you last looked" with Mark as seen, coverage badges,
  Needs attention (ranked, nested, "I'm on it" with a note for admins,
  links to logs / capture / site checks, silenced behind a toggle, an
  explicit "nothing needs attention"), Coming up, AI spend (24 h against a
  usual day, subscription apart, unpriced count), alerts that keep coming
  back (5+ firings in a week, median duration, no threshold suggested),
  resolved in 24 h, an incident history dialog. Refreshes every 30 s.
- Forecasts (`internal/forecast`, `internal/server/forecasts.go`, every
  10 min): disk time-to-full from 7 days of free space at 30-minute steps;
  Theil-Sen slope (median of pairwise slopes, thinned to 200 points);
  restart after a lasting rise (median of the next 6 samples above the
  previous 6 by 2% of the size or 256 MB; a spike that comes back down is
  not one); at least 72 h since then; the last 24 h and the whole must
  give times to full within a factor of 3; shown within 30 days, "soon"
  within 7; filesystems under 1 GB and pseudo file systems left out.
  Certificates expiring in 14-30 days; agents limited in the last hour.
- AI spend: a 1-minute loop over `ai_cost_usd_per_minute`; absent when
  nothing writes it. Noisy alerts from `incident_events`.
- Verified in the browser against a local server with alerts posted into
  its Alertmanager (no stores there): ranking, nesting, acknowledge,
  history, silenced toggle, "some forecasts could not be made". The
  forecast math is tested on synthetic series only; not yet against a
  filesystem that really fills.
- Not built: AI budget burn (there is no budget setting), memory and other
  trends as watch items, grading of security findings, ownership.

## AI explanations of incidents (built 2026-10-24, uncommitted)

Step 3 of the Home page plan.

- `internal/ai`: a `Provider` interface (system prompt, one user message,
  a JSON Schema; returns JSON and token usage) and the Anthropic client on
  the official Go SDK (`anthropic-sdk-go`): structured outputs through
  `output_config.format` (current models reject forced tool use), effort
  from the config, the system prompt cached, the server-side refusal
  fallback (`fallbacks: "default"`) on the models that take it.
  `option.WithoutEnvironmentDefaults()`: an `ANTHROPIC_API_KEY`,
  `ANTHROPIC_BASE_URL` or credential profile in the server's environment
  is ignored (the SDK reads them by default; found by its own warning in a
  test). The key file is read on every call.
- Config `ai:` (provider, model default claude-opus-5-5, effort default
  low, api_key_file, base_url, data, monthly_budget). Refused at load
  without a budget, or when the model has no price in `costs.tokens`.
- `internal/server/evidence.go`: the bundle by incident kind (incident,
  history, other open incidents of the host, the alert rule, the agent,
  profile changes in the 6 h before, metric summaries of the hour before
  per kind, detections without command lines, log excerpts only with
  `data: [logs]`); every text masked again (the agent's built-in rules and
  e-mail addresses); 4 000 characters per item, 24 000 per bundle.
- `internal/server/explain.go`: table `explanations` (the evidence as
  sent, answer, model, tokens, cost, latency, verdict); preview, explain
  (admin), list, verdict (anyone), `GET /api/v1/ai`, `PUT
  /api/v1/ai/paused` (setting `ai_paused`). Claims citing ids that were
  not sent are marked unsupported, uncited ones interpretation. Budget per
  calendar month (UTC) from stored costs. Cost into the metric as
  `vigil_ai_cost_total{model}` (written with the price series) and the
  `ai_costs` rule `source="vigil"`.
- Home page: Explain per incident (admins), the preview of what would be
  sent, the answer with evidence that opens on a click, verdict with a
  note, AI status and Pause.
- Verified: unit and server tests against a fake Messages API, and in the
  browser against a local fake API (preview, send, the unsupported-claim
  mark, cost, verdict). Never run against the real API: no key on the
  trial yet.
- Not built: explanations made by themselves when an incident starts,
  the staged-incident evaluation set, other providers, an outbound preview
  for metadata-only installs that differs from the stored record.

## Scrape tokens from a file (built 2026-10-24, uncommitted)

- `bearer_token_file` on a static scrape job: the agent reads the token
  from a file on its own host at every scrape (`internal/scrape`), so the
  token never passes through the server or a profile and a new one needs
  no restart. One of `bearer_token`, `bearer_token_file`, `basic_auth`.
  First used on the trial for an application whose metrics need a token.

## Separate binaries (built 2026-10-25, merged)

Steps 2 and 3 of the Foundations build order in direction.md.

- `vigil-agent` (agent, `enroll`, `events`, `claude-hook`, `version`; with
  no subcommand it runs the agent), `vigil-server`, `vigil-relay`, each in
  `cmd/` with its commands in `internal/cli/{agentcli,servercli,relaycli}`.
  `vigil` keeps `apply` and `tracegen` (`internal/cli/opscli`) and, for one
  release, every old subcommand, logging a warning (`claude-hook` silently).
- One version for all: `internal/version.Version`, set with `-X`.
- The leak: `inventory` imported `fleet` for one struct, which pulled the
  rule engine and Alertmanager (with an AWS SDK and Kafka) into the agent.
  `Inventory` is now in `internal/model`. linux/amd64 stripped: agent 21.0
  MB (26.5 with the leak), relay 7.7, server 49.9, `vigil` 51.1.
- Guards: `internal/depcheck` and a test in `cmd/vigil-agent` and
  `cmd/vigil-relay` that fail on server packages and libraries
  (`go list -deps`); `make size` checks the agent (23 MB) and relay (9 MB)
  budgets, and CI runs it.
- Dockerfile targets `agent`, `server`, `relay`, and `all` (the default,
  the old image). `deploy/` and the docs use the new binaries; Install has
  "From `vigil` to the separate binaries".
- Verified in the vigil-test VM: all four image targets build; the compose
  stack runs the `server` target; on the Mac a `vigil-agent` direct, one
  through `vigil-relay` (shown with its `via`) and an old `vigil agent`
  (with the warning) all report and send host metrics; `vigil-agent enroll`
  enrols against it; the docs anchor resolves and the served `fleet.js`
  carries the new commands (the Fleet page itself was not opened signed in).
- On the trial since 2026-10-25 (server, host agent, OTLP gateway). Agent
  memory (`vigil_agent_memory_bytes`, average over an hour): host agent 45
  MB against 60–66 MB with `vigil agent` the day before, gateway 28 MB
  against 43–47 MB.
- The home agents moved the same day: two run `vigil-agent`; a NixOS host
  runs the new `vigil agent` (with the warning) until its own NixOS config
  points at `vigil-agent`, as `deploy/vigil-agent.nix` does.
- gRPC out of the agent (2026-10-25, uncommitted): the generated OTLP
  collector packages, which bring grpc and grpc-gateway only for their
  service stubs, are replaced by `internal/otlpwire` (the three export
  requests as field 1 lists, byte-identical to the generated types, tested
  both ways). Agent 21.0 → 15.6 MB, idle memory with OTLP on 27.3 → 25.1 MB
  (macOS); `cmd/vigil-agent`'s guard now also forbids grpc, grpc-gateway and
  the collector packages; the agent's size budget is 17 MB. What remains of
  genproto is two message packages cel-go needs.

## Context and Explain for a log line (built 2026-10-25, merged)

- Explore → Logs, an opened line: **Context** (everyone) and **Explain…**
  (admins, with `ai` set up). `internal/server/logcontext.go`: `POST
  /api/v1/loglines/context` with the line as the store returned it (needs
  `_time`, `agent`, `_stream_id`). Lines like it: same source (container,
  pod, unit or app, else the stream) matching `linePattern` (the Go twin of
  `suggestPattern` in explore.js, tested to agree), counted 1 h / 24 h /
  7 d by agent with first and last; 10 lines before and after in the
  stream; ±10 min on the host: agent starts (`vigil_agent_start_time_seconds`
  every 15 s), container stops and starts, restarts, OOM kills, profile
  changes; the agent's incidents open then (`Tracker.Around`); notes for
  "says error but level is info", "only one in 7 days", "the agent started
  N s before/after".
- `internal/server/logexplain.go`: the context becomes the bundle (the line
  and its neighbours as class logs, the rest metadata; the pattern is the
  line's text, so it is withheld too without `data: [logs]`); its own
  prompt and schema with `assessment` (harmless, needs_attention,
  unknown). Refused (409) without `data: [logs]`. Cached by agent, source,
  pattern and day (`explanations.subject = log:<hash>`, a column added
  then): a second ask returns the stored answer at no cost; `again` makes a
  new one. The shared steps of incident Explain moved into `explain()`,
  `aiReady()` and `explanations()`.
- Found while building: the trial's "error" in dockerd's `request cancelled
  by client … broken pipe` (status 499, level info) was the agent being
  restarted mid-request by a deploy; once in 7 days.
- Verified: server tests with fake stores and a fake model API (context,
  notes, cache, refusal, nothing sent without logs); in the browser against
  a local server with the test VM's stores, an agent tailing a file and a
  fake model API: the panel, the preview, the answer, the cache (one call
  for two asks), a verdict, Explain again. Not against the real API.

## Secrets from OpenBao or Vault (built 2026-10-25, merged)

- `internal/secrets`: an OpenBao/Vault client (AppRole or token login,
  renewal at a third of the token's life, a new AppRole login at its
  maximum, `lookup-self`; KV v1 or v2 told by `sys/internal/ui/mounts`;
  read, `LIST`, `sys/capabilities-self`; values cached 5 min in memory, the
  last good value given while the store fails) and `Sealer` (AES-256-GCM
  with `data_dir/secrets.key`, made 0600 on first save, bound to a label).
- `internal/server/secrets.go`: settings `secret_store` (address, CA,
  namespace, auth, role_id, auth mount, KV mount), `secret_store_cred`
  (the secret_id or token, sealed), `secret_refs` (use → path and field).
  Admin API under `/api/v1/secrets`; saving or removing the store asks for
  the password again (the admin token for a token sign-in), and the saved
  credential is reused only for the same address, login and CA. No API
  answers with a value: found, version, last four characters. `vaultLoop`
  every minute: `vigil_secret_store_up`,
  `vigil_secret_store_token_expiry_seconds{renewable}`; built-in alerts
  SecretStoreDown and SecretStoreLoginExpiring.
- The first use is the AI key: `ai.api_key_file` is optional now; without
  it the key comes from the use `ai_api_key`. `ai.Provider` gained
  `Verify` (lists models, no cost) and takes a `KeyFunc`.
- UI: Secrets (admins): the store card with Test (KV version, policies,
  token life, may list / write / read each chosen path), Test and save,
  Remove; the table of uses with Choose… (picks from the mount's list when
  allowed) and Test (found, version, …last4, provider accepted). Docs:
  docs/secrets.md (what to create in OpenBao), API, metrics.
- Verified: unit tests against a fake OpenBao (renew, re-login, last good,
  KV1, capabilities, list, sealer), a server test (nothing in clear in any
  data file, credential not reused for another address, Explain with the
  store's key, survives a restart, remove), and in the browser against a
  real OpenBao 2.4.1 dev server in the test VM: set up, test, choose,
  provider accepted; sealed: reported down with OpenBao's reason, the key
  still served from memory; after a restart while sealed the key is
  missing (documented); unsealed: recovered by itself.
- Not built: writing values from vigil, the receivers' tokens from the
  store, a server audit log entry for the changes (only the server log).
- On the trial since 2026-10-25: the key is read from an OpenBao that was
  already there, over a network shared with vigil's server only; Explain on
  a log line ran there against the real API (about 3,200 tokens, 2.5 cents,
  8 s; asked again, answered from the cache).

## Time range picker (2026-10-25, merged)

- `internal/ui/static/js/timerange.js` (`TimeRange`, `RANGES` moved there):
  one picker for Explore (Metrics, Logs, Log volume), dashboards, Services
  and Sessions. Quick ranges with a search; absolute From-To through the
  browser's `datetime-local` (local time zone, shown); the last five
  absolute ranges in localStorage (`vigil.recentRanges`); « and » shift by
  the range's length, − doubles it; a range reaching now becomes the
  matching quick range again. The URL keeps `from`/`to` or `last` as
  before. Opens leftwards near the right edge, as a fixed panel under
  700 px. Checked in the browser against a local server (no stores: another
  project's Podman machine was running, so vigil-test could not start), and
  by the user on the trial.

## Docker log streams that stall (2026-10-25, merged)

- Seen on the trial: after the host agent started during a deploy (Docker
  was rebuilding and recreating the server and gateway containers), all 53
  container log streams sat 16 minutes with nothing from Docker (a SIGQUIT
  goroutine dump: every stream in IO wait), while journald kept flowing; a
  restart resumed from the positions and lost nothing.
- `internal/logs/docker.go`: a watchdog per stream closes it after `Idle`
  (10 min) without a line; the next scan (`Rescan`, 10 s) reopens it from
  its position. A container first seen without a position now remembers
  the time it was opened, so reopening a quiet one never starts at its
  first line. Test `TestDockerStalledStreamIsReopened` (fails without the
  watchdog). On the trial host agent since 13:21 UTC.

## Hosts that sleep (2026-10-25)

- Docs (Alerts): a laptop agent pages on every lid close, so the rule file
  can carry an alert named HostDown (it replaces the built-in one) that
  leaves out agents whose `vigil_agent_info` had `role="workstation"` in the
  last week, plus WorkstationSilent after 72 h. `vigil_agent_up` carries
  only `agent`, hence the join on the agent's own info series. On the trial
  in its rule file since 2026-10-25 (the Mac had come and gone 19 times in
  a week).

## Toolchain and version in the interface (2026-10-26)

- `go.mod` pins `toolchain go1.26.10`, and `x/net` and the OpenTelemetry
  trace exporters moved to their current releases. `govulncheck ./...`
  reports nothing our code calls; run it before each release. CI takes the
  toolchain from `go.mod`. A local Go newer than the pin is used as it is
  (`GOTOOLCHAIN=auto`), so builds meant to match CI set
  `GOTOOLCHAIN=go1.26.10`.
- `/api/v1/me` carries the server's `version` (signed-in callers only), and
  the sidebar shows it under the user and role.

## Boop integration, first part (built 2026-10-29, uncommitted)

Against Boop's observability contract v1 (Boop Jump repository,
`docs/observability.md`); see direction.md "Boop integration".

- **`exported_agent`:** a scraped or OTLP series' own `agent` label is
  renamed by the agent before sending (`exportAgentLabel` in
  `internal/agent/agent.go`); the server's `extra_label` had overwritten
  it, folding every Boop Jump agent of a scrape into one series. A log
  line's `agent` field or stream label is kept as `exported_agent` by the
  server (`forwardLogs`) instead of being dropped.
- **OTLP tokens and TLS** (`internal/otlp/auth.go`): `otlp.tokens_file`
  ("name token" lines, re-read on change every 5 s at most, a broken file
  keeps the old tokens, none means nobody in), 401 without a token;
  `emitter=<name>` as a stream label on logs and a label on metrics, a
  sender's own `emitter` kept as `attr_emitter` / `exported_emitter`;
  `otlp.tls.cert_file`/`key_file`, re-read on change. Spans get no emitter.
- **Integrations** `boop` (Boop /metrics and tenant export, audit-event
  rules pinned by `event_agents` + `emitters` + `service_instance`,
  tenant_ref check, break-glass, the shadow-access rule),
  `boop_jump_gateway`, `boop_jump_agent` (Boop's alert names; counters as
  differences with `offset`, not `increase()`), and `sshd` (journald unit
  templates for ssh/sshd units, an anchored pattern giving `ssh_*` fields;
  not detected, since sshd is everywhere). Test data copied from Boop Jump
  into `internal/fleet/testdata/boop/`; `boop_test.go` checks every metric
  name against the contract and classifies the contract's real sshd lines
  with the rule's own conditions.
- **Shadow access:** `SshLoginNotThroughBoopJump` on `_msg:~"^Accepted "`
  under an sshd unit on `guarded_agents`, unless key type `-CERT`, the
  Key ID grammar and `ssh_ca_fp` (the line's last field) equal to
  `tenant_ca` all hold. Unparsed Accepted lines count as not Jump. No
  serial check (a log rule cannot compute it).
- Verified live in the Lima host (Ubuntu 24.04, OpenSSH 9.6): a test CA
  and a rogue CA trusted by sshd, a certificate with the contract's Key ID,
  a plain key, `logger -t sshd` spoofs; Boop's endpoints played by the
  contract's fixtures; the contract's OTLP requests and attack cases sent
  with and without tokens. Jump logins never alerted; plain-key, rogue-CA
  and Lima's own logins did; spoofs stayed under `session-N.scope`;
  forged instance id, other token and wrong tenant raised their alerts;
  three Boop agents stayed three series. All four dashboards checked in
  the browser. Not tried against a real Boop or Jump.
- Found on the way: `increase()` on a counter seen for the first time
  fired `BoopEventsDropped` from a static fixture; the Lima Ansible role
  still installed the old `vigil` binary for a unit that runs
  `vigil-agent` (fixed in the git-ignored tools).
- **Map panel** (`type: map`, `internal/ui/static/js/map.js`): queries
  are nodes (`node` + `tier`, optional `name`, `group`, warn/crit), links
  (`from` + `to`, `live` animates a value above zero) or details (`of`);
  a node is label name + value; colour = worst of node and details; up to
  20 queries, 300 nodes; SVG columns by tier, groups, hover card with
  `columns` labels. Shown to admins only (the interface hides it; the
  series stay queryable). The panel form keeps a map's queries untouched.
  `TestBuiltinIntegrationDashboards` now checks every integration's
  dashboard against `parseDashboard` (the map's 14 queries passed the
  integration tests while the server would have refused them). Checked in
  the browser: layout, colours, live links, hover card, a viewer's note,
  editing a map in the form.
- An alert in the rule file now replaces an integration's alert of the
  same name (it did only for built-in rules; integrations were replaced
  only by group name, so the trial's copy of Boop's rules would have
  doubled every Boop alert). `internal/server/integrations.go`, tested in
  `TestIntegrationsAndDetection`.
- Not built: session markers, the request-access link, `host_id`,
  seq-gap and duplicate detection, the 4 KiB record limit, roles (RBAC).

## Next

- Foundations, build order in `direction.md`: step 1 on paper (data
  classes, tenant model, envelope, retention, integrity claims), reviewed
  by a second model; step 3 (CI budgets) can run beside it; step 4 (mTLS,
  envelope, spools) waits for step 1.
- At phone width the page is wider than the screen (637 px at 375 px).
- Open from the original plan: AI investigator and MCP server (Claude API
  or local models?), and the watched eBPF window on the trial host.

## Never run for real

- Any kernel older than 6.12; there is no fallback below 5.8. (The x86-64
  probe loaded and worked on Debian 13, kernel 6.12, on 2026-10-10: all five
  event types, container names, about 33 events a second on a busy Docker
  host, no measurable change in host CPU.)
- Kubernetes on a multi-node cluster, or with a runtime other than containerd.
