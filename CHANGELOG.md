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
  incident, previewed and with a monthly budget.
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
