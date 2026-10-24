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
