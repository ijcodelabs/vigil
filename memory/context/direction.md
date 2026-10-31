# Direction

What the owner has said the project should become. These are intentions,
not commitments, and most are not started.

- **Scale:** thousands of plain hosts and thousands of Kubernetes clusters.
  The first deployment is a few hosts.
- **Config:** layered profiles chosen by label selectors, resolved on the
  server, editable both on the server and from git.
- **OpenTelemetry as the APM** (agreed 2026-10-10). OpenTelemetry's
  auto-instrumentation replaces a commercial APM agent for Java, Python and
  the like; the screens are ours to build. Planned order:
  1. a local OTLP endpoint in the agent for metrics and logs, labelled like
     everything else and stored in the stores already in use;
  2. traces: a trace store (VictoriaMetrics' own is the candidate; check its
     maturity first), request rate, errors and latency per service computed
     from spans and stored as ordinary metrics, and a trace view;
  3. a Services page and a service map; log lines linked to their trace by
     trace id, chart spikes linked to example traces;
  4. eBPF-based instrumentation in the agent for HTTP and gRPC without
     touching the application.
  Plan for trace volume (sampling that keeps errors and slow requests) and
  for showing "running but not instrumented" from the process detection.
- **Fleet security, later:** encrypted agent traffic, and the server issuing
  credentials itself (certificates, enrolment tokens, one-time passwords).
  Remote sites may not reach the server directly, so a relay mode stays on
  the list.
- **AI-lab-style fleets:** monitoring GPU clusters and model serving
  (accelerator health, job and queue awareness, per-model serving figures).
- **FinOps for AI:** tracking token usage and cost. All sources add up in
  one metric, `ai_cost_usd_per_minute` (built 2026-10-24; see state.md).
- **AI gateway in the agent, later** (agreed 2026-10-24, after forecasting
  and network monitoring): the agent opens a local endpoint per provider
  (Anthropic, OpenAI, ...), applications are pointed at it with the base-URL
  variable their SDK already reads (`ANTHROPIC_BASE_URL`,
  `OPENAI_BASE_URL`), and it forwards each request unchanged while reading
  the token usage from the answer, streamed answers included. That counts
  cost for any language and SDK without a code change, by service, into
  the same cost metric; later it could hold a budget or limit per service.
  It sits in the path of every call and every API key passes through it,
  so it must add little latency, fail in a way that is easy to bypass, and
  never log a key or prompt text (the agent's redaction applies). For apps
  you own, OpenTelemetry GenAI instrumentation stays the first choice;
  the providers' usage and cost APIs stay the way to check the totals
  against the invoice.
- **AI investigator and MCP server:** deliberately last, because building
  the rest was expected to change what it should be. It now starts as the
  Home page and its incidents (below).
- **SaaS as well, maybe** (raised 2026-10-25): many tenants, some with
  thousands of hosts, next to self-hosted. The foundations below are
  designed so that it stays possible.
- **SLOs and error budgets** (picked 2026-10-30 as the first gap against
  what other monitoring products show): SLOs as objects over probes, spans
  and any counters, multi-window burn-rate alerts, SLA reports per month.
  Step 1 (SLO objects and their per-minute counts) is built; the spec is
  [slo.md](slo.md).

## Foundations: data pipeline, storage, modules, DORA (agreed 2026-10-25)

Four designs, each reviewed with ChatGPT in three rounds (separate designs
first, then each challenged the other's); these are the points both ended
up agreeing on. They come before new features: the wire protocol and the
store layout are the most expensive things to change once thousands of
agents run mixed versions. Three shapes are designed for: small
self-hosted (one server, one binary), large self-hosted (thousands of
hosts and clusters, one organisation) and SaaS (many tenants).

### Data pipeline: agent → relay → server → stores → archive

- **Four data classes**, each with its own delivery, priority, retention
  and loss rules: telemetry (metrics, logs, traces; may be sampled or
  dropped), security (detections, captures), audit (compliance evidence)
  and change (rollouts, deploys, package changes; feeds the ledger). Today
  everything is one class: a single spool drops the oldest batch when full.
- **A versioned batch envelope** for every class: tenant (set by the server
  from the credential, never from a label), logical agent id, installation
  and credential epoch, class and stream, batch id, sequence range, event /
  observed / received times, schema version, integrity fields (hash chain).
  Event ids only for security, audit and change; telemetry is tracked by
  batch and sequence range.
- **The promise:** no loss within declared capacity (a spool budget and
  priority per class); beyond it a durable loss record (what, how much,
  when) wherever possible. A missing loss record proves nothing.
- **The agent's own health is evidence:** kernel ring-buffer loss, probe or
  source failures, rule suppression, spool pressure, truncated captures,
  clock state, coverage. Without it "no detection" proves nothing.
- **Identity:** mTLS client certificates from vigil's CA (`internal/pki`),
  renewed automatically, revocable, rotated; the logical agent id apart
  from the installation's identity; designed for agents that are offline
  for a while. TPM or workload attestation later. Relays authenticate to
  the server too; `X-Vigil-Relay` stays display only; the original agent
  address comes through a trusted chain.
- Retries with jitter and the server's retry hints; overload shed by class
  priority; cardinality admission and fairness between tenants next to the
  byte and sample limits; detections and captures get limits too.
- Redaction also covers stream labels; its rules are versioned and a masked
  value records which rule masked it.
- **Cluster agent:** a PVC for its identity, spool, position and
  observations; any interruption writes a coverage-gap record. A Kubernetes
  watch for rollouts; a re-list restores state, never the transitions it
  missed. The Kubernetes API audit log is delivered on its own path, not
  through that agent.
- **Server:** durable receipts and the outbox for audit, security and
  change; telemetry gets bounded deduplication plus a sequence watermark
  saved periodically (whether VictoriaMetrics drops duplicate samples
  depends on `-dedup.minScrapeInterval`: check, do not assume). An
  append-only **server audit log**: sign-ins, users, roles, tokens,
  enrolment and credentials, profiles, rules, rollouts, silences,
  acknowledgements, exports, capture access, retention and legal-hold
  actions, deletions, AI Explain sends, storage settings; each with actor,
  decision, before/after hashes, request id, tenant and sequence. Switching
  audit off is itself audited and alerted.
- Clocks: agent time and server receive time are both kept, implausible
  clocks quarantined, NTP state reported as a fact.
- **Audit sources,** first-hand on the hosts and clusters vigil runs on:
  auditd (primary where present) and sessions (logins, sudo, ssh), packages
  and binaries, Kubernetes API audit, and file integrity monitoring
  (baseline of paths with hash, owner, mode; a change source such as
  fanotify or eBPF; rehash; re-baseline; a loss model) as a module, because
  it needs extra privilege. eBPF exec/setuid/open enrich auditd; they do
  not replace audit or FIM. Everything else (cloud IAM, identity provider,
  CI/CD, source control) arrives through authenticated, classified generic
  receivers (syslog, webhook, OTLP) that keep provenance; per-vendor
  connectors only as modules once a user needs one. Receiving a webhook
  does not prove its source was complete.
- **Integrity claims by shape:** small self-hosted shows internal
  consistency, and tampering only against a checkpoint kept outside (a
  customer-held export, a timestamp service); large self-hosted and SaaS
  can claim tamper resistance when object lock and the signing key are
  administered apart from vigil. Not "immutable" for all three. vigil gives
  evidence for controls (change management from DORA, incident response
  from incidents, coverage from Fleet, the audit log), not compliance.

### Storage

- Small self-hosted: SQLite stays (one binary, one process, backups). Large
  and SaaS: Postgres (HA) for all relational state; server replicas keep no
  state; background loops are leased, with a fencing token checked at every
  side effect.
- Domain stores (`IncidentStore`, `FleetStore`, `LeaseStore`, `DoraStore`,
  ...), not a generic SQL layer; SQLite and Postgres implementations where
  they differ; migrations and invariants tested on both.
- Tenant scope is a model through keys, unique constraints, authorization,
  caches, object paths and audit records, not a column. New tables (ledgers,
  observations) are tenant-qualified from the start; the existing 23 get
  the model before SaaS. Row-level security is defence in depth.
- **Transactional outbox:** a change and its event in one transaction;
  idempotent publishers to VictoriaLogs (a searchable copy) and the
  archive; neither is needed for correctness. No direct dual writes.
- VictoriaMetrics, VictoriaLogs and VictoriaTraces are hot stores on block
  storage; their object-storage support is backup only, and retention is
  per instance. So hot retention is short, and long retention per class
  lives in the archive.
- **Archive:** Parquet on object storage (pure-Go writer), organised by
  accepted ledger sequence ranges; per-tenant prefixes and envelope keys;
  signed manifests sealed by risk (e.g. hourly for audit) with a sealed
  watermark; WORM (object lock) for audit; legal holds override expiry;
  reconciliation ledger → hot → archive; restore tests; no compaction
  across tenants. Deletion covers manifests, retry queues and backups; a
  tenant's key is destroyed only after retention ends; tombstones record
  redactions. Captures kept short and access-controlled. A customer's own
  bucket is an enterprise export target. Iceberg or DuckLake later.
- **DuckDB:** never in core (it needs CGO). An optional, isolated job over
  one tenant's archive, with limits and short-lived credentials, behind a
  governed analytics service (also for "Ask vigil" over history); never
  attached to the production Postgres.
- **Alerting state:** vigil's own notification coordinator (silences,
  grouping, deduplication, routing, attempts, retries, audit, outbox) on
  SQLite or Postgres becomes the one path. An external Alertmanager cluster
  is a bridge for large self-hosted; the embedded Alertmanager stays in
  small installs until the coordinator matches the supported routing
  features and has passed restart and failover tests.
- **SaaS:** cells (Postgres, Victoria* with `AccountID`/`ProjectID`
  tenants, server replicas); a small global database maps tenant → cell;
  large tenants get a cell of their own; a cell is also the answer to data
  residency.

### Modules

- "Small" means memory, CPU and what runs as root first; artifact size is a
  release budget too. Measured 2026-10-25 (linux/amd64, stripped): `vigil`
  is 51 MB; an agent-only binary was 26.5 MB, still pulling in
  Alertmanager, the AWS SDK and Kafka through agent → inventory → fleet →
  rules → alerting. Fixed the same day by moving `Inventory` to
  `internal/model`: `vigil-agent` is 21.0 MB, `vigil-relay` 7.7 MB, and the
  agent's idle memory with host metrics only is 27 MB against 41 MB for
  `vigil agent` (macOS). Then without gRPC (the OTLP collector packages
  replaced by `internal/otlpwire`): 15.6 MB, idle 25 MB with OTLP on.
- Integrations stay YAML data (detection, config, rules, dashboards); only
  bundles from an outside catalog are signed.
- Compiled into the agent only if mature, used by most agents, light on
  dependencies and needing no extra privilege; the runtime-security eBPF
  probe stays. Modules: eBPF HTTP/gRPC instrumentation, GPU vendor
  collectors, the AI gateway, native database SDKs, FIM, DuckDB analytics.
- Built-in capabilities are inert until a profile switches them on: no
  goroutines, listeners or eBPF programs.
- A module is a separate executable on a private Unix socket with a small,
  versioned, typed protocol; not Go's `plugin` package; WASM only for user
  parsers later. The agent supervises it and mediates everything it sends
  (labels, limits, timestamps, redaction, spool, class). A module declares
  its sources, data class, schema, privileges, health checks and retention.
- First-party modules ship with the agent's version, signed, checksum
  verified before they run; a module's security fix is an agent patch
  release. Protocol and schema versions in the handshake.
- Profiles request a module; the host's local config allows module names
  and privilege classes (`modules.allow`), so the server alone can never
  start a module that proxies API keys.
- Activation states: pending, downloading, verified, starting, healthy,
  failed, rolled back. Saving a profile shows which agents can take it;
  only eligible agents are staged.
- Artifacts hosted by the server, explicit import for offline sites; relays
  stay stateless (mirrors only when bandwidth needs them). Modules run
  unprivileged by default (own user, `no_new_privs`, sandbox, limits);
  eBPF modules get only the capabilities they need.
- Server features (DORA, catalog, AI) stay compiled in and inert until
  enabled; no separate services for the sake of modularity.
- First module: GPU (exists, low risk); second: the AI gateway.

### DORA

- Deployment frequency, lead time for changes, change failure rate and
  failed deployment recovery time, per service and team, never per person.
- Immutable Change, Release, Rollout and RolloutStep records; one
  production deploy is one rollout, however many cells or pods it reaches.
  Reported rollouts are keyed by the producer's rollout id plus attempt,
  detected ones by a deterministic workload / target / version id; the
  image digest is a relation, not a key.
- Detection: a Kubernetes watch on workload identity, pod-template hash and
  rollout progress, with persisted checkpoints and Argo/Flagger status
  where present. Rollout observations are Kubernetes telemetry (change
  class), recorded whenever the kubernetes integration is on, DORA or not,
  so DORA backfills from the retained observations when it is enabled.
  Plain-host and systemd changes are low-confidence evidence.
- Detected deploys count without setup (`source=detected`, with
  completeness); a CI report adds to the same record and never overwrites
  it. CI reports with a token kind of its own (deploy reporting only,
  scoped to tenant / service / environment, own rotation, never enrols
  agents).
- The ledger is relational (SQLite, Postgres later) with tenant-qualified
  events and the outbox to the archive; DORA metrics in VictoriaMetrics are
  versioned projections that can be recomputed.
- Production targets are fixed when a rollout starts; autoscaled instances
  belong to their parent target; a new cell or cluster is a separate target
  expansion. Frequency counts completed production rollouts under a
  versioned completion policy.
- Lead time from CI-provided provenance (the revisions actually in the
  artifact), measured from merge to trunk; vigil never clones a repository.
- Service identity starts from labels and an OWNERS-style file, then becomes
  a catalog object (stable ids, aliases, release units, owner history)
  edited through the UI/API and `vigil apply`.
- Failure attribution by scope (frozen rollout targets against the
  incident's affected resources) and ordering, keeping competing rollouts.
  Confirmed needs a person's link or an automatic rollback whose recorded
  reason names the rollout; a single candidate is likely; several are
  ambiguous and count against none. The headline change failure rate uses
  confirmed links, with likely and ambiguous shown next to it, so a low
  confirmed rate with many likely links reads as unknown. AI Explain may
  propose a link; it never counts until confirmed.
- A canary rolled back before users were affected is a caught release, not
  a failure.
- Recovery time: incident opened → resolved, labelled as such; sustained
  SLO recovery added where an SLO exists.
- Deploy markers on charts, on the Home page and in AI Explain bundles come
  early, before the numbers.
- Raw evidence stays in its region; release identities and compact ledger
  facts replicate for a global view.

### Build order

1. On paper: data classes, tenant model, envelope (sequence, reset and drop
   rules), retention and legal hold, integrity claims per shape.
2. Separate `vigil-agent`, `vigil-server` and `vigil-relay` binaries; fix
   the agent's import leak.
3. CI budgets: forbidden agent imports, size, idle memory, CPU, goroutines,
   open files and listeners, startup time; wire-protocol compatibility
   fixtures, mixed-version tests, fault injection, load and retry tests.
   Check that built-in capabilities are inert until enabled.
4. Identity and durable edge state: mTLS enrolment, renewal, revocation,
   installation epochs; the envelope and per-class spools on the agent; the
   cluster agent's PVC and gap records.
5. Rollout observations with a real watch (change class).
6. Domain stores and the outbox: audit, change and security records,
   receipts, the server audit log. The Postgres implementation before
   large installs or SaaS.
7. Audit sources: auditd and sessions, packages and binaries, Kubernetes
   API audit on its own path, generic classified receivers.
8. Hot stores and the archive from the outbox: manifests, sealing tiers,
   reconciliation, restores, retention, holds, tombstones.
9. The module system (manifest, signatures, allowlist, supervisor, mediated
   ingestion, activation states), proven with GPU; then FIM as a module;
   the AI gateway after it.
10. DORA on the change ledger: deploy markers and frequency first, then CI
    reports, incident linking (confirmed / likely / ambiguous), lead and
    recovery time, the catalog.
11. Later: server replicas with leases and fencing, the notification
    coordinator, cells for SaaS, governed archive analytics with the
    optional DuckDB job.

## Home page, incidents and AI explanations (agreed 2026-10-24)

When someone opens vigil, the first page says what needs attention, what
will soon, and what happened since they last looked. It works without AI;
the AI explains incidents on top. The design was reviewed with a second
model (ChatGPT, two rounds); these are the points both ended up agreeing on.

Order:

1. **Incidents and the briefing index.** Signals (firing alerts, silent
   agents, failing checks, security detections, later forecast crossings)
   become incidents. Key: kind, primary resource (host, container, check,
   service) and a discriminator (alert fingerprint, check, rule plus
   novelty key, forecast series), so two unrelated alerts on one host stay
   two incidents. Every source signal is kept as evidence. Correlation by
   containment only: a host-down incident is the parent of that host's
   container, check and alert incidents and pushes them down while it
   lasts, without erasing their history. Lifecycle: open, acknowledged
   (who, when, comment), resolved when every signal has cleared after a
   grace period per source, reopened within a set time instead of a new
   one; "no data" is not "resolved". The briefing is a table kept up to
   date in the background, read on page open; heavy store queries only on
   drill-down. Coverage ("5 of 5 agents reporting"), silences in effect,
   and a per-user "seen up to" marker (an ordered event id, moved by "mark
   seen" to the snapshot actually viewed) belong to it.
2. **The Home page** on that index, ranked incidents first, an explicit
   "nothing needs attention", real spend apart from estimates and unpriced
   use, and noisy-alert tuning candidates (counted from incident history,
   with that history linked; no threshold suggested from a count alone).
   Built alongside: strict forecasts only, each with a range and the chart:
   disk time-to-full (slope computed by the server from fetched samples,
   at least 3 days of data, reset after a cleanup or resize, two horizons
   that must agree, else a watch item), certificate expiry, AI budget burn,
   agents at their ingest limits. Memory creep and security trends start
   as watch items until they prove reliable.
3. **AI Explain per incident.** A bounded evidence bundle chosen by
   incident type, not an agent that runs its own queries. The answer has a
   fixed shape (summary, hypothesis, confidence, evidence ids, unknowns,
   next checks), every factual claim cites an evidence id, vigil rejects
   ids it did not send and labels uncited text as interpretation. Bundle
   content is untrusted data, never instructions. Cached by incident
   fingerprint and a hash of the bundle; model, prompt version, cost and
   latency recorded; its own spend in `ai_cost_usd_per_minute`; a monthly
   budget. Providers behind one interface: Anthropic first (owner's
   choice), an OpenAI-compatible client later; the answer's JSON Schema
   kept to what both can produce. Privacy: metadata only by default; the
   "logs" data class (masked excerpts chosen for the incident, size-capped,
   masked again on the server) switched on explicitly, as on the trial;
   captures, command lines, prompt and tool content and anything
   credential-like never sent. Outbound preview, an audit record of every
   send, a kill switch. Evaluation: every explanation stored with its
   bundle and the operator's verdict against criteria fixed beforehand,
   plus staged incidents on a test VM (OOM kill, disk fill, crash loop,
   bad config, simulated miner, thin evidence, misleading log text) with a
   held-out part, replayed on every prompt or model change.
4. **Later:** the MCP server and "Ask vigil" on the same bounded, read-only
   evidence service: a question box on the Home page for anything about
   the infrastructure, telemetry and incidents, answered from vigil's own
   data (owner's wish, 2026-10-24). The AI gateway; connectors (the
   briefing to Telegram, Slack, ... through the existing receivers first).
5. **Later: guard against a cost blow-up** (owner's wish, 2026-10-24): a
   security check that someone reusing our API keys, or a runaway client,
   cannot run up a bill unnoticed. Starting points: spend jumps per key,
   service and person from `ai_cost_usd_per_minute` and the providers'
   usage APIs (calls from keys or places never seen before), budgets that
   alert before the provider's own limit, keys found in logs by the
   agent's masking, and the AI gateway's per-service limits once it
   exists.

Security findings are graded policy / suspicious / indicator, show
novelty and scope ("first seen, on 1 of 400 hosts") instead of a verdict,
and need baselines and allowlists (the trial's postgres health check read
its own /etc/shadow 261 times in 45 minutes). Mining-pool connections,
stratum, known bad domains and suspicious process ancestry count as strong
evidence; a file name alone does not. CVE matching (Trivy, Grype, OSV over
the image inventory) is later.

## Views from a prompt (proposed 2026-10-31, not agreed)

The owner's idea: a prompt field on the Home page. "Show me a map of the
whole fleet", "show me nginx 5xx for shop-api by path": the AI builds a
view, the owner asks for changes ("make that per host", "drop the CPU
panel") until it is right, then saves it as a dashboard, so opening it
again needs no AI call. A first release is built (state.md, 2026-10-31):
the ledger and steps 2 to 6 below; the real-model evaluation set is not.
Reviewed with a second model
(Codex, two rounds, 2026-10-31); the points below include its corrections.
Entry point: the Home page (owner's choice, 2026-10-31, over the review's
suggestion of the dashboard list). The draft opens from there; Save puts
it in the dashboard list like any other.

- **The AI writes a dashboard, not a page.** Its answer is the dashboard
  JSON vigil already stores (title, variables, panels with PromQL and
  LogsQL queries; the time range and refresh are not part of it), held to
  a JSON Schema with existing panel types only. No generated HTML or code.
- **Checking is a separate, limited preflight.** `parseDashboard` checks
  shape only: it does not parse queries, check variables against them or
  run them. The preflight parses each query and runs it on a short fixed
  range with a deadline, a series cap, a query-count cap and a log-range
  cap, and says per query: valid, valid with no data, invalid, too costly.
  "No data" is not "invalid" (a quiet service is real). Run again on save.
  It cannot tell a query that answers the wrong question; that is what
  the evaluation set and the operator's review are for.
- **A small trusted catalog, not the fleet.** The model gets the JSON
  contract, a few built-in dashboards as examples, metric names with type,
  unit and label keys from vigil's own integrations, and the log fields
  and LogsQL forms vigil supports. Not every series, label value or host
  name: on thousands of hosts that is huge, and it would send the
  inventory out. A host or service the operator names is found by the
  server, which asks the operator to choose when there are several, and
  sends only the chosen id. Names, labels and log fields are written by
  whoever runs the hosts: they go in as delimited data, never as
  instructions. No log lines, command lines, captures or credentials.
- **Admins only to start**, like Explain: today any viewer can query and
  only admins save dashboards, and a draft must never show what its user
  could not query.
- **Each turn returns the whole dashboard**, not a patch; the server
  computes and shows the difference. Drafts are stored on the server per
  user, with revisions, and expire after a day.
- **Saving makes an ordinary dashboard**, editable by hand, linked in the
  audit record to the draft and prompt rather than carrying the prompt.
- **Explain's plumbing, made shared.** `internal/ai`, the pause switch and
  the monthly budget are there, but the budget and `vigil_ai_cost_total`
  sum only the `explanations` table (`internal/server/explain.go`), so AI
  calls need one ledger first. Each turn records caller, draft revision,
  catalog version, prompt version, model, tokens, cost, the request as
  sent, the answer and the preflight results.
- **Evaluation from the start:** fixed prompts against a synthetic fixture
  (good basics, ambiguous asks that must ask back, unknown metrics, too
  broad LogsQL, injection text in host names and labels, viewer vs admin),
  checked by required selectors and panel types rather than exact JSON,
  replayed on every prompt, model or catalog change.

- **The model never writes a selector for a chosen host.** It returns
  the query with a slot for an opaque reference (`resource:r7`) the server
  handed out; vigil substitutes the quoted matcher after validation, so
  the model cannot widen or invent the operator's pick. Resolution in the
  first step covers only what SQLite knows (agents, enabled integrations),
  with a result cap and the operator choosing; not label-value listing,
  which today goes unbounded to VictoriaMetrics.
- **What is not bounded today:** the browser queries through plain
  reverse proxies (`internal/server/server.go`) with no deadline, series
  cap or range policy; dashboards trim what they draw (40 series, 200
  rows) after fetching all of it; `logQuery` caps returned rows, not the
  scan; the server's HTTP client has one 30 s timeout. So the preflight is
  its own server-side path with a short deadline, small caps, bounded
  concurrency and an allowlist of LogsQL forms per panel type (no `tail`,
  no unanchored regex), never the public proxy routes. Real limits on the
  stores' own work need their settings or a separate query endpoint.
- **The catalog is mostly written by hand.** vigil records no metric
  type, unit or help at ingest; `/api/v1/metadata` is passed through to
  VictoriaMetrics and not normalised. So: a curated metric file (meaning,
  unit, labels) from `docs/metrics.md` and the built-in integrations, plus
  starter dashboards as examples (about 150 KB of sources, much less after
  trimming), version-stamped. Locally changed integrations are not sent
  automatically.
- **Also needed in the first release:** cancel, a per-user cooldown and a
  global limit, retries keyed by draft revision and request, expiry
  cleanup, failed calls counted in the ledger, the Home control renamed
  from "AI explanations" to "AI features", a docs page (what is sent,
  limits, cost, retention), and clear states for no provider, paused,
  budget spent, rejected and no data.
- **Evaluation in two layers:** `go test` with a fake provider (as
  `explain_test.go` does) and small fixture stores, free and in CI; and an
  opt-in run against the real model on the synthetic fixture, with its own
  budget cap, never in CI.

First step: admins, up to a few `timeseries`, `stat`, `table` and `logs`
panels, no variables, no maps, nothing sent but the catalog, explicit
Save. Maps come later and only from server-owned topology templates; the
map panel exists but needs explicit node and link queries.

Ideas for what it could show. Realistic with today's data: log lines with
their rate (nginx 5xx by path if those fields are collected), host and
container views, checks, AI cost, SLO burn from the SLO series, `sudo` and
`docker exec` as filtered logs. The rest needs data or models vigil does
not have yet (a topology graph, a change timeline, baselines, deploy
events, process trees) or is a data-reading feature rather than a
dashboard (chart explanations, the briefing):

- Fleet: the topology map (relay, regionals, hosts, containers, services)
  coloured by open incidents and SLO burn; what changed in the last day
  (deploys, config, new containers, agent upgrades) on one timeline; hosts
  that stopped looking like their peers; disks by days left.
- Logs: a log panel with its rate chart; log patterns new since the last
  deploy; errors next to deploy markers.
- Security: a host's process tree for the last hour with the unusual
  executions marked; outbound connections to destinations never seen
  before; `sudo` and `docker exec` as a timeline.
- SLOs, DORA, cost: error budget burn for every SLO, worst first; DORA per
  team this quarter against the last; AI spend per model with the month's
  forecast against the budget.
- Lighter: a morning "fleet weather" paragraph with four panels;
  "explain this chart" on any panel; when the fleet is quiet; the hosts
  that make the most alerts per useful incident.

## AI modules: Claude Code first (agreed 2026-10-19)

Watching AI coding agents is a family of integrations named
`ai_module_<tool>` (`ai_module_claude` first), each switched on and off by
profiles like any integration. The Sessions page is shared by all of them
and shows when any `ai_module_*` is in use.

`ai_module_claude` takes Claude Code's own OpenTelemetry output
(`CLAUDE_CODE_ENABLE_TELEMETRY`): cost and token metrics, one event per API
call, prompt and tool call, and (beta) traces with the subagent tree. Plan:

1. Record a real session with a subagent on a local server; settle what
   Claude Code really sends (empty event bodies, GenAI span attributes).
2. Agent: event name as message when the body is empty;
   `otlp.metrics.drop_labels`; `otlp.resource_labels`; no double count
   between `claude_code_*` and `genai_*`.
3. The integration (rules: over budget, runaway session, API error bursts;
   a dashboard; saved queries) and `docs/claude-code.md`.
4. Sessions page from events (list by session, prompts in order, cost by
   main/subagent and agent name), then the subagent tree from traces.
5. Redaction in the agent before storage; `vigil claude-hook` for an audit
   trail of tool calls and permission decisions.
6. Later: compare with the Claude Code Analytics API.

Defaults: `user_email` stays a metric label; `session_id` does not. Whether
prompt text is stored is open: it is recorded locally first, to see what it
holds, and decided after. Viewers would see it, because log queries pass
through to the store.

## Boop integration (outline agreed 2026-10-29; first part built, see state.md)

Boop (access requests and approvals) and Boop Jump (recorded server and
Kubernetes access) are a sister product. Each works alone; they meet only
through Boop's published observability contract (Boop Jump repository,
`docs/observability.md`, test data in `testdata/o11y/v1/`): Prometheus
metrics, audit events as OTLP logs, links into Boop. No shared code. Vigil
builds against the contract's test data and only after the owner's go.

- **A `boop` integration:** scrape profile (Boop's tenant export, gateways,
  agents), dashboard, Boop's alert rules under the same alert names.
- **A Map panel**, generic (nodes from `*_info` series, edges from series
  with two ends, colour by joined queries), first for Boop's control plane →
  gateways → agents → resources, later also the APM service map. **Admin
  only in v1** (owner's decision). Hiding the panel does not hide the
  series: viewers can still query them, which the docs must say.
- **Later, real roles (RBAC):** which role may see, use or change what (the
  map, Boop's logs, dashboards, Explore by metric or stream), enforced in
  the server's query path, not only in the interface. Not now.
- **Rules from the joint review:** Vigil's copy of Boop's events is for
  display and correlation; nothing that grants access, approves or clears an
  alert relies on it. No approval that depends on an incident's state.
  Boop data is accepted only from pinned Vigil agents; the agent's OTLP
  endpoint gets a per-emitter token and TLS before events come from another
  host. No people's ids, local users or command text in Vigil or in AI
  Explain bundles.
- **Shadow access:** a log rule on sshd's Accepted lines (journald, by
  systemd unit), anchored at both ends, that tells a Boop Jump login (a
  certificate from the tenant's CA with the contract's Key ID) from any
  other. Best effort; what it does not cover is listed in the contract.
- **`host_id` fact:** systemd's app-specific machine id with the app id in
  the contract; a hint for joining hosts, never an identity. Duplicates are
  flagged, not merged.
- Order: integration → request-access link on incidents → chart
  annotations with session markers and an Explain evidence item → Map →
  `host_id` → shadow access → OIDC sign-in (Boop then grants roles for a
  time) → compliance evidence, which waits for the audit data class.
