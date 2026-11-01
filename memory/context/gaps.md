# Gaps against other observability products: what goes into vigil, what goes into Boop

Proposed 2026-11-01, not agreed. Compared with what commercial
OpenTelemetry-based observability products offer in late 2026 (SLOs, AI
coding-tool insight, tracing, APM, infrastructure, language set-up, logs,
alerting). Reviewed once by a second model
(Codex); its corrections are folded in and marked where they changed the
plan.

## The rule for the split

- **vigil observes and explains.** Anything that collects, stores, queries,
  shows or alerts on telemetry is vigil. vigil's AI stays read-only:
  explanations, drafts the operator saves, read-only query interfaces
  for AI tools.
- **Boop acts, with approval.** Anything that changes a system, needs
  access to one, or runs an AI agent that writes code goes through Boop:
  Core (just-in-time access and approvals), Jump (recorded sessions),
  Chat (workrooms where an agent works in a branch and a person approves
  its plan).
- They meet only through public interfaces, as the Boop integration
  already does: vigil's query and alert APIs, webhooks, the observability
  contract, links. No shared code. Nothing that grants access or approves
  relies on vigil's data (rule from the joint review, direction.md).
- vigil enforces its own authorization for anything done to vigil, also
  when Boop asks for it; a Core approval can be an installation's policy
  on top, not a replacement (review).

This is how an "AI SRE" agent splits here: finding the problem is vigil;
fixing it, drafting a PR or changing a silence is Boop.

## Where vigil already stands

At par or close: Prometheus rules and Alertmanager, PromQL, SLOs with
multi-window burn-rate alerts, error budgets and SLA reports (slo.md,
steps 1–5), Services with RED from spans, log-to-trace links, the Claude
Code module (sessions, cost by part, cache tokens, tool events, hook
audit). Single-line log patterns exist (`suggestPattern`, `linePattern`).
Ahead: eBPF runtime security, fleet config with profiles and staged
rollout, relay, own CA, self-hosted only.

## Order (both passes, after review)

Foundations step 1 (on paper) and step 3 (CI budgets) stay first, as in
direction.md; they gate everything that changes ingestion. The items
below run beside them where they do not.

1. **Service map and service-page views.** Map panel fed by
   `traces_service_calls_total`; on the service page, tabs for HTTP
   routes, database calls and outbound calls from stored span attributes,
   RED per row. No entity graph needed.
2. **Notification delivery status and the coverage report.** Per
   receiver: last success, last failure and its error, a built-in rule
   for a receiver that keeps failing. Coverage: services and detected
   software with no alert, SLO or dashboard; "running but not
   instrumented" from detection. Makes alerting trustworthy.
3. **Trace evidence: tail sampling and exemplars.** Whole traces kept on
   error or latency (relay or gateway agent), exemplars from kept spans to
   chart points. Depends on Foundations step 4 (envelope, spools).
4. **Triage comparison**, after 3: a selected set of spans (heatmap brush,
   error spike, incident) against a baseline, top attribute values with
   lift; then the same over logs by field and pattern. Bounded windows,
   chosen keys, value limits, deadlines, cancellable, says when a result
   is partial or sampled. Becomes an Explain evidence item. (Review: forbid
   free scans over VictoriaTraces.)
5. **Log patterns over a range**: template mining across a time range with
   counts and "new since"; extends the single-line pattern.
6. **Languages: docs and runtime integrations.** A page per language
   (Python, Java, Node, Go, .NET): upstream OpenTelemetry
   auto-instrumentation pointed at the agent, log correlation. Dashboards
   and rules from OpenTelemetry's runtime conventions, detected by
   `telemetry.sdk.language`. Docs for Fluent Bit, Vector, Logstash and the
   Collector into the agent's OTLP endpoint. No own SDK.
7. **AI tool insight across harnesses**: normalise Codex, Cursor and
   Copilot CLI through stable OpenTelemetry GenAI conventions (no plugin
   per vendor yet); per MCP server calls, p95 latency and failures across
   harnesses; cost per merged PR only on the DORA change ledger.
8. **Pipeline rules and telemetry cost**: drop/keep, logs → metrics,
    roll-ups on the agent and relay; volume and cost per agent, profile
    and service. Shape agreed with Foundations step 1 first.
9. **Deploy markers** on SLO, service and dashboard charts, after
    Foundations steps 5, 6 and 10 (review: guessed markers from
    `service.version` or image changes assert deployments that did not
    happen). The exception is vigil's own staged rollouts, which vigil
    knows for certain; those can be marked earlier.
10. **Later, with the module system**: continuous profiling (eBPF),
    eBPF HTTP/gRPC instrumentation, LLM evals on sampled GenAI spans,
    cloud metrics (CloudWatch first), checks from several regions.

## Parked (owner, 2026-11-01)

- **Read-only MCP server.** Not now: it is not clear yet whether AI tools
  will keep using MCP or something else. What any such interface needs
  underneath (bounded, typed queries and evidence bundles with scopes,
  quotas, audit and redaction) is the lasting part.
- **Interchange** (OpenSLO, Perses and Grafana export, `vigil apply` for
  the rule file): to be talked about later.

## Cut or not planned

- Entity model and resource map across hosts, pods, services and
  databases as a programme: a large identity product. Service map first;
  narrow links from inventory later, as bounded materialised facts with
  provenance, never live joins across the stores (review).
- Visual query builder: the query text stays the truth; templates,
  saved queries and "alert from this query" cover most of it (review).
  Two-level alerts (warning and critical in one go) can still come from
  Explore's alert form.
- Bulk import of third-party alert catalogs: curated, tested rules per
  integration instead (review).
- Guessed log severity: only explicit mappings and structured fields
  (review).
- SLOs drafted from a prompt: views-from-prompt makes dashboards, not
  policy; later, with stronger checks (review).
- Coding-agent skills as a vigil feature: install docs stay current;
  shareable skills belong to Boop Chat (review).
- Adoption or productivity by person, hashed user ids included: a stable
  hash is still a personal identifier. Team and service only. If ever
  allowed, per installation, salted and rotatable, short retention,
  excluded from Explain, AI interfaces and export, never ranked (review).
- Session replay in the browser; RUM is an open question.
- Terraform provider, marketplaces, per-volume pricing: only if the SaaS
  happens.

## Boop

Each item is Boop's decision; this is what vigil would expect to serve.

- **Alert cards in Boop Chat.** Acknowledge: a narrowly scoped vigil
  call, no Core approval by default. Silence: Chat can ask, vigil
  authorises and audits; a Core approval is an installation policy.
  Request access: wholly Core and Jump. No "open incident" button: vigil
  makes incidents from firing alerts (review).
- **Scoped write tokens in vigil** for those calls: audience, tenant,
  exact action and resource, short expiry, idempotency key, replay
  protection, who asked through whom, audit. Card payloads are untrusted
  and re-authorised on the server (review).
- **Workroom investigations**: an incident offers "investigate"; a
  workroom agent reads vigil through a read-only interface, proposes a fix and
  works in a branch a person approves. Access, if needed, through Core
  and a recorded Jump session. Waits for the parked AI interface.
- **Workroom runner telemetry into vigil**: the runners emit
  OpenTelemetry GenAI and `vcs.*` attributes under a versioned public
  contract, so work done through Boop shows in vigil's AI views. No
  account ids, prompt or tool content, or command text (review).
- **Jump session markers on vigil charts**: already in the Boop order.
- **Sign-in and roles**: vigil OIDC with roles Boop grants for a time
  (already in the Boop order). Compliance packs can read vigil's SLA
  reports and incident timelines through the API, after the audit data
  class. Whether `boop install` also installs vigil: open.

## Questions for the owner

- Is the split rule (vigil reads, Boop acts) right, with vigil enforcing
  its own authorization?
- Agree the first three: service map with service views, delivery status
  with coverage, tail sampling and exemplars?
- RUM: build it, or leave browsers out of vigil?
- Scoped write tokens for Boop's alert cards: now or later?
