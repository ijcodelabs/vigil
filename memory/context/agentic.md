# vigil for agentic companies

Agreed as a direction by the owner 2026-11-01. vigil's part of the first
slice is built (state.md); Boop's part is not.
Reviewed once with a second model (Codex); its corrections are in.
Boop's side of the same plan lives in the Boop repository.

The picture: most work in a company is done by AI agents; people watch,
plan with the agents and hand work to an agent or to a person. vigil has
to serve agents as readers of its data, watch agents as a workload, and
show people what is going on. It does not become the place where work is
assigned or approved: that is Boop. The parked items (MCP server,
dashboard import and export) stay parked; see [gaps.md](gaps.md).

## Principles

1. **vigil observes, explains and verifies; Boop acts.** Unchanged from
   the Boop integration in direction.md.
2. **Agents are visible in vigil, people are not.** vigil keeps agent ids
   and task types; no person ids. Boop's observability contract v1 carries
   `actor_ref` and `approver_ref` (per-tenant pseudonyms); whether v2 drops
   them for vigil is open (a stable pseudonym is still a person id).
3. **vigil's signals can only restrict.** A signal from vigil may make
   Boop stricter than its own baseline policy, never looser, and is never
   needed for a grant, an extension, an approval or recovery from a
   restriction. If vigil is down, a signal has expired, has a bad
   signature, an unknown version or the wrong tenant, Boop falls back to
   its baseline policy; manual and emergency paths keep working.
   Signals carry issuer, tenant, subject, scope, sequence, issued and
   expiry times; Boop decides and records its decision; relaxing needs a
   Boop-side policy check or a person. Minimum hold times against
   flapping.
4. **Agents read vigil through narrow capabilities**, not a general
   bearer token: scoped to an incident's services and time range,
   with quotas, redaction and audit. The existing read-only API tokens are
   too broad for this.
5. **Coverage is explicit.** "No signal", "telemetry incomplete", "agent
   unavailable" and "verified healthy" are different answers and are
   shown differently.
6. **No rankings** of people or agents. Agent figures are about safety and
   reliability of a kind of task, not productivity.

## First slice (1–2 weeks)

- **Our own stack traced.** OpenTelemetry in vigil-server and in Boop
  (the Boop side does Core, Jump and Chat). One trace across Chat → Core
  → Jump gateway → agent, with a propagation test. No message text,
  approval content, commands, tenant or user ids in spans.
- **Golden-path docs for Python and Go** (`docs/`): upstream
  auto-instrumentation or SDK pointed at the agent, log correlation
  (`logging`, `slog`), checked against the Boop services. Runtime
  dashboards and rules only for what this proves; TypeScript after.
- **Incident evidence snapshot**: a bounded, redacted endpoint that
  returns an incident's evidence bundle (the one Explain already builds)
  for a short-lived capability issued per workroom run. This is what
  Boop's "Investigate in workroom" reads.

## Then, in order

1. **Work events from Boop** (contract v2): item transitions, approvals,
   grants, Jump sessions, workroom runs and receipts. Stored as logs of
   class "change", shown as chart markers and on incident timelines.
   They are certain because Boop recorded them; later they become rows of
   the change ledger (Foundations step 6, DORA step 10).
2. **Service map from traces with service views**, notification delivery
   status (gaps.md items 1 and 2).
3. **Verification**: given a service or SLO and a change time, compare
   bounded windows before and after (RED, burn rate, error logs, kept
   traces) and return a verdict (better, worse, no change, not enough
   data) with its limits. Advisory: a person or Boop's policy acts on it.
4. **AI cost per service, per trace and per work item**, labelled as an
   estimate (sampling, retries and caching make it one); the item key
   travels as a span attribute. Budgets are alerts, not SLOs.
5. **Tail sampling and exemplars** (after Foundations step 4), then the
   triage comparison and log patterns (gaps.md).

## Later, when a real workflow needs it

- **Claimed against observed**: a workroom run bound to a cgroup, systemd
  scope or container that the runner creates, with its start time; eBPF
  exec, connect and open inside it compared with what the agent reported.
  Corroboration only: a runner-reported PID can be reused or forged, and
  a missing observation is a coverage gap, not proof.
- **Agents as entities** in vigil: per agent and task type, runs
  finished, time, cost, interventions, rollbacks.
- **The restrict feed** of principle 3 (SLO budget spent, active incident
  on a service, a claimed/observed mismatch).
- **Control room** on the Home page over the same events: what agents and
  people are doing (from Boop's events, without person ids), what changed
  since I last looked, budgets and SLOs; actions open Boop.

## Foundations step 1 gains

The actor model (agent ids, no person ids), the "change" and "work" data
classes, the evidence bundle schema and its redaction, retention for
prompts, artifacts and AI summaries.
