# Changelog

What changed in each release of vigil, for the people who run it. A change
that users notice adds a line under **Unreleased** in the same pull
request; a release renames that heading to its version and date. Sections,
when there is something for them: Added, Changed, Fixed, Security, and
Upgrade notes (what to do, in which order, what goes away).

Versions are `vX.Y.Z`. Before 1.0, a minor release (`0.2.0`) brings new
features and may need the steps in its upgrade notes; a patch release
(`0.2.1`) only fixes. A server accepts agents of its own minor version and
of the one before, so the server can be upgraded first and the agents
after. How a release is made: [releasing.md](memory/context/releasing.md).

## Unreleased

### Added

- **Portun dashboard** for the whole installation, one row per part: Core
  (also removals waiting or stuck, approved but not granted, shards,
  broken recordings), Jump (from the gateways when there is no tenant
  export), Chat, versions, and the `portun-*` containers with their errors.
- **`portun_chat` integration:** Portun Chat's `/metrics`, alerts when it or
  its database is down, its backup is over 26 hours old, Web Push fails or
  webhook posts are refused, and its own dashboard.
- **Portun Jump gateway:** Portun Jump's newer rules (connections limited,
  recording quota, gateway CA expiring or its rotation left open, one agent
  identity in two places) and panels for them.
- **Home** shows the AI spend of the last 30 days and the last year under
  the last 24 hours, with the date the total starts from when the metric
  store keeps less than that.
- **Long-term history:** with `storage.history_url`, a second
  VictoriaMetrics, the server keeps hourly rollups for years: AI cost, SLO
  counts, host CPU, memory and filesystems, and site checks. On its first
  start it fills them in from what the main store keeps. Rule groups can
  write there with `store: history`, dashboard panels read it ("PromQL
  (history, hourly)"), and the AI costs dashboard has two History panels.
- Time ranges of 30 days, 90 days and a year.
- The **Logs and security** dashboard counts failed SSH logins: a total,
  a chart per agent, and the addresses and user names tried most. It reads
  sshd's lines in the journal and needs no integration.
- **Send test** on each integration under Alerts → Contacts & routing →
  Delivery: a `VigilTest` message through that integration only, with the
  answer shown at once. Delivery now lists every integration in use, also
  before it sent anything.
- **Who would be told:** the labels of an alert in, the contacts the
  routing sends it to out (route, grouping, waits, silenced now), each with
  a test. In the editor it works on the text before it is saved. API:
  `POST /api/v1/alerting/test` and `/api/v1/alerting/route`.

### Changed

- **Boop is Portun now:** the integrations `boop`, `boop_jump_gateway` and
  `boop_jump_agent` are `portun`, `portun_jump_gateway` and
  `portun_jump_agent`, and read Portun's names: metrics `portun_*`, audit
  events `portun.*` (logger `portun.audit`), Jump Key IDs `portun-jump:v1:…`.
  Their jobs are `portun` and `portun-export`, their alerts `Portun…` (the
  names in Portun's own rule files), their default token files
  `/etc/vigil/portun-metrics-token` and `/etc/vigil/portun-export-token`.
  The `sshd` integration masks Portun Jump's Key IDs. A profile that still
  names an old integration is refused with the new name to use.

### Upgrade notes

1. **If a profile uses `boop`, `boop_jump_gateway` or `boop_jump_agent`,**
   upgrade the server, then rename them in the profile (their parameters
   stay the same; check the token files, which had `boop` in their default
   names). Until then, the Fleet page shows the error with the new name and
   the agents that profile matches keep the configuration they have. Portun
   sends only its new names, so the old integrations see nothing from it
   anyway.
2. Dashboards installed from the old integrations stay as they were;
   delete them once the Portun ones are installed. Saved queries and rule
   files that name `boop_*` metrics or `boop.audit` need the new names too.

### Security

- The server, the relay and the agent's OTLP endpoint close connections
  that stay idle for 2 minutes or take too long to send a request (5
  minutes; 1 for OTLP). Before, a client could hold a connection open for
  ever, and enough of them shut everyone out.
- The binaries listen and connect with plain TCP instead of Multipath TCP,
  which Go turns on by default and which the known exploit of the Linux
  TCP flaw CVE-2026-98432 goes through. This does not replace a fixed
  kernel on the host.
- Built with **Go 1.27.4**, which fixes GO-2026-6859 in `net/http`, code
  vigil's server, agent and relay use.

## v0.2.0 (2026-11-02)

### Added

- **Fleet → Identities** and `vigil identity list|approve|revoke|delete`:
  manage the names that may hold a certificate from the server's authority
  without calling the API by hand. An endpoint's certificate is renewed as
  soon as its approved names change.
- **Client certificates for the OTLP endpoint:** the server's authority
  signs short-lived (7-day) certificates for names an admin approves;
  agents renew them for programs on their host (`identity.clients` in the
  agent's own file) and for their OTLP endpoint (`otlp.tls.from_server`),
  which can require them (`otlp.tls.client_certs`). Revoking a name refuses
  every certificate issued for it at the next request. See APM, "Client
  certificates from the server".
- **Evidence grants:** a short-lived token that lets one run of an AI agent
  (a Boop Chat workroom, for instance) read one incident's masked evidence
  bundle and nothing else, at most 20 times; issued by an admin or by a new
  kind of token that may only issue grants. Listed and ended on the Users
  page. See the API docs, "Evidence grants".
- **The server traces itself:** with the standard `OTEL_EXPORTER_OTLP_*`
  variables set, `vigil-server` sends a span per API request (method,
  route and status only) and continues a caller's W3C trace context.
- **Service map:** Services → Map draws services by how deep in the calls
  they sit, with databases and outside hosts last; lines and boxes turn
  yellow or red with their share of failures.
- **Did a change help?** A service's page, and `GET /api/v1/verify`,
  compare the same time before and after a moment on requests, failures
  and the 95th percentile (or an SLO's bad events), with a verdict and its
  limits.
- **Markers of Boop's work on charts:** Boop Chat's item, investigation and
  workroom-run events (observability contract v1) are drawn on a service's
  charts and its SLO's charts; click one to check whether the change helped.
  An incident an investigation read shows its Boop item's events.
  `GET /api/v1/markers`.
- **`otlp.logs.drop_attributes`:** leave attributes out of incoming log
  records; the `boop` integration drops Boop's person pseudonyms with it.
- **SSH logins:** the person pseudonym inside a Boop Jump certificate's Key
  ID is masked before it is sent (`boop-jump:v1:masked:…`).
- **AI cost per service, trace and work item:** the Services list and a
  service's page show what its model calls cost; a trace page shows its
  model calls and their cost; spans carrying `work_item.key` add up per
  work item (`GET /api/v1/costs/spans`). Estimates at `costs.tokens`.
- **Agents keep every model call that reports tokens**, whatever the
  sample share, so cost per trace and per work item is not lost to
  sampling.
- **Notification delivery:** Alerts → Contacts & routing → Delivery shows,
  per contact and integration, messages sent and failed since start, the
  last success and the last error (addresses cut to their host); metric
  `vigil_notifications_total` and built-in alert `NotificationsFailing`.
- **Guides for Python and Go services:** OpenTelemetry set-up with log
  lines linked to their traces (`docs/apm-python.md`, `docs/apm-go.md`).

### Changed

- **Agents older than the server** skip settings of a managed configuration
  they do not know, apply the rest and say so on the Fleet page
  ("skipped: …"), instead of refusing all of it and taking no change until
  upgraded. Settings about TLS, tokens, certificates, keys, identities,
  redaction or dropping data still refuse the whole configuration. See
  Fleet, "Agents older than the server".
- **Boop integration:** follows Boop Core's renamed metrics (`aps_*` →
  `boop_core_*`, same labels); alerts and dashboards keep their names.
  Upgrade Boop Core with it: older Cores still send `aps_*`.

### Security

- **Pinned build inputs:** every GitHub Action is pinned to a commit and
  every container image (the build image, `deploy/docker-compose.yml`, the
  examples in the docs) to a digest, so a moved tag cannot change what
  vigil is built from; CI refuses an unpinned one, runs `govulncheck`, and
  Dependabot proposes updates weekly. The vendored UI libraries are recorded
  with their checksums (`internal/ui/VENDOR.md`).
- `golang.org/x/net` 0.62.0 (fixes GO-2026-6860 and GO-2026-6866, in code
  vigil does not call), with `x/crypto`, `x/sys`, `x/sync`, `x/text` and
  `x/tools` moving up one minor version each.
- Built with **Go 1.27.3** (the build image and the toolchain; building
  from source still needs Go 1.26 or newer); OpenTelemetry 1.47,
  Prometheus client 1.25, Alertmanager 0.34.2, SQLite 1.61 and other
  dependencies updated. The compose example runs VictoriaMetrics v1.155.0
  and VictoriaTraces v0.12.1.

### Upgrade notes

1. **If you use the `boop` integration, upgrade the agents first, then
   the server.** Its configuration now carries `otlp.logs.drop_attributes`,
   which v0.1.0 agents do not know: they refuse the whole managed
   configuration and keep their old one until upgraded. This once goes
   against the usual order (server first). From v0.2.0 on, agents skip a
   setting they do not know and say so on the Fleet page, so it does not
   happen again.
2. **Boop Core and the `boop` integration go together:** the integration
   reads Boop Core's metrics as `boop_core_*`. With a Boop Core from before
   that rename (it still sends `aps_*`), Boop's alerts and dashboards see
   nothing until Boop Core is upgraded too.
3. **Client certificates are off until you set them up.** They need the
   server's own authority (`tls: {self_signed: true}`); nothing changes for
   an existing installation that does not approve any names. The server
   adds its new tables itself at start.
4. Built with Go 1.27.3; building from source still needs Go 1.26 or newer.

## v0.1.0 (2026-10-31)

The first release. vigil was used from source until now; this is what it
holds.

### Added

- **Binaries:** `vigil-agent` on every host or Kubernetes node,
  `vigil-server`, `vigil-relay` for sites that cannot reach the server, and
  `vigil` for the operator tools (`vigil apply`). Static, for linux/amd64
  and linux/arm64 (`vigil` also for macOS), with checksums and a keyless
  signature; container images on ghcr.io.
- **Home:** incidents with a history (acknowledge, resolve, reopen), what
  changed since you last looked, what is coming up (disks filling,
  certificates expiring), AI spend, and optional AI explanations of an
  incident, previewed and with a monthly budget; admins can ask for a view
  in words, which the model writes as a dashboard and vigil checks before
  drawing it.
- **Collecting:** host and container metrics, Prometheus scraping
  (including Kubernetes pods by annotation), process groups, systemd units,
  GPUs, OpenTelemetry (metrics, logs, traces); logs from files, journald,
  Docker and Kubernetes with parsing templates, a log volume page and drop
  rules; site checks (HTTP, TCP, TLS, ping); runtime security events from
  eBPF with rules evaluated on the host and captures; SSH logins and the
  Boop integration. Secrets are masked on the host before they leave it.
- **Alerting:** Prometheus-format rules on metrics and logs, the essential
  ones built in, Alertmanager inside for routing, contacts and silences.
- **Service level objectives:** over site checks, spans or any counters,
  counted once a minute into `slo_good_per_minute` and
  `slo_total_per_minute`; saved through the API or `vigil apply` (a
  `slos` directory) or on the SLOs page. Multi-window burn-rate alerts
  (`SLOBurnFast`, `SLOBurnSlow`) and `SLOBudgetExhausted`, shown as SLO
  incidents on Home. The SLOs page shows each one's budget left, burn
  rates, coverage, incidents and history; Home lists nearly spent budgets
  under "Coming up"; a starter dashboard "SLOs". SLA reports per month
  or quarter (achieved, met, budget used, worst day, coverage, incidents)
  as a page to print or save as PDF, as CSV and through the API; a
  finished period's report is stored and does not change. SLO templates
  offer an SLO for each check, traced service and HAProxy backend;
  integrations can ship their own (`slos:`).
- `vigil tracegen -token` and `-token-file`, for an agent that requires
  OTLP tokens.
- **AI and costs:** Claude Code telemetry, sessions and an audit hook;
  language-model calls counted and priced from OpenTelemetry spans; GPU
  costs; one cost metric for all of it.
- **Fleet:** profiles selected by labels and kept in git, integrations,
  staged rollouts, enrolment with the server as its own certificate
  authority, relays, limits per agent.
- **Interface:** Home, Dashboards, Explore with saved queries, Services,
  Sessions, Alerts, Captures, Fleet, Users, Secrets (OpenBao or Vault);
  Grafana data sources through read-only tokens; an API for everything.

### Upgrade notes

- From a build of the source with the single `vigil` binary: switch to
  `vigil-agent`, `vigil-server` and `vigil-relay` as described in
  [Install](docs/install.md#from-vigil-to-the-separate-binaries). The old
  `vigil agent`, `vigil server`, `vigil relay`, `vigil enroll` and `vigil
  events` still work in this release, with a warning, and are removed in
  0.2.0.
- From `v0.1.0-rc.1`: replace the binaries or images. The server adds its
  new tables (SLOs, the record of AI calls, drafts of views) when it
  starts; explanations made before keep counting against the month's AI
  budget.
