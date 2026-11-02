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
