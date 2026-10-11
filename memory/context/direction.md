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
