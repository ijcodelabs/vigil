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
- **FinOps for AI:** tracking token usage and cost.
- **AI investigator and MCP server:** deliberately last, because building
  the rest was expected to change what it should be.

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
