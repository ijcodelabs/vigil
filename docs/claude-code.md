# Claude Code

What Claude Code costs and what it does: spend and tokens by person,
model and subagent, sessions, tools, refused tool calls and API errors.
vigil takes Claude Code's own OpenTelemetry output; nothing is installed
next to it.

This is the first of the AI modules, integrations named `ai_module_<tool>`
that watch AI coding tools. They are switched on and off like any
integration (see [Fleet](fleet.md)).

## Switch it on

On the server, a profile that switches the integration on for the machines
where people run Claude Code. The agents report a running `claude` process
by themselves (`detected_ai_module_claude`):

```yaml
select:
  - detected_ai_module_claude="true"
priority: 15
integrations:
  ai_module_claude: {}
```

This opens the agent's OpenTelemetry endpoint on `127.0.0.1:4318`, starts
the rules and installs the "Claude Code" dashboard.

Then point Claude Code at the agent, in `~/.claude/settings.json` for one
person, or in Claude Code's managed settings for everyone on the machine
(so that it cannot be switched off):

```json
{
  "env": {
    "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
    "OTEL_METRICS_EXPORTER": "otlp",
    "OTEL_LOGS_EXPORTER": "otlp",
    "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
    "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"
  }
}
```

It has to be one of those two. A project's `.claude/settings.json` or
`.claude/settings.local.json` is only obeyed by Claude Code running
interactively in a terminal: `claude -p` and the sessions of the Claude
desktop app ignore telemetry settings from there, even in a trusted folder.
(Tried with Claude Code 2.1.299 and 2.1.303.)

Sessions of the desktop app report as `claude-code-desktop`, the terminal
as `claude-code`; the dashboard and rules take both. To split spend by
team, give the agent a label (`labels: {team: platform}` in its file or a
profile): every metric and event it passes on carries it.
`OTEL_RESOURCE_ATTRIBUTES` in Claude Code's settings works for the
terminal, but the desktop app sets that variable itself and the app's
value wins.

## What arrives

| | What | Where |
|---|---|---|
| Metrics | `claude_code_cost_usage_total` (USD), `claude_code_token_usage_total` (`type`: input, output, cacheRead, cacheCreation), sessions, active time, lines of code, commits, pull requests; by `model`, `query_source` (main, subagent, auxiliary), `agent_name`, `user_email`, `team` | metric store |
| Events | one log line per API call (`claude_code.api_request`, with cost, tokens, duration), API error, prompt, tool call and decision, finished subagent; with `session_id` and `prompt_id` | log store |
| Traces | optional, beta in Claude Code: each prompt as a trace, subagents nested under the tool call that started them | trace store |

The integration leaves the session and account ids that Claude Code puts on
every metric point out of the metrics (`otlp.metrics.drop_labels`): with
them, every session would be a new series. Sessions are in the events.

Cost is what Claude Code computes at API prices. On a subscription that is
what the same use would cost through the API, not what is billed.

Traces need two more settings in Claude Code and a trace store in vigil
(see [Services and traces](apm.md)):

```json
"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
"OTEL_TRACES_EXPORTER": "otlp"
```

## Content: prompts, commands, tool output

Claude Code sends no content unless told to. Each of these settings adds
some, and each is a decision about what the people who can read vigil may
read:

| Setting | Adds |
|---|---|
| `OTEL_LOG_USER_PROMPTS=1` | the text of prompts (events and traces) |
| `OTEL_LOG_ASSISTANT_RESPONSES=1` | Claude's replies |
| `OTEL_LOG_TOOL_DETAILS=1` | tool inputs: shell commands, file paths, the first part of a subagent's instructions |
| `OTEL_LOG_TOOL_CONTENT=1` | tool output (traces only): what commands printed, what files held |
| `OTEL_LOG_RAW_API_BODIES` | whole API requests, the conversation included |

`OTEL_LOG_TOOL_CONTENT` is the dangerous one: when Claude Code reads a
`.env` file, its contents are the output. The agent masks secrets in
everything it passes on (see [Secrets in what is sent](configs.md)), but
masking recognises common shapes of keys and passwords, not everything a
file may hold. Everyone who can sign in to vigil, viewers included, can
read events and traces.

## Alerts

| Alert | When | Parameter (default) |
|---|---|---|
| ClaudeCodeOverBudget | a person's use cost more than the budget in 24 hours | `daily_budget` (50 USD) |
| ClaudeCodeRunawaySession | one session cost more than this in 15 minutes | `runaway_usd` (5 USD) |
| ClaudeCodeApiErrors | more API errors than this from one machine in 5 minutes | `error_burst` (10) |

Parameters are given where the integration is switched on, for example
`ai_module_claude: {daily_budget: "20"}`. The rules run once for the whole
server, so when several profiles give values, the profile with the highest
priority decides.

## The dashboard

Spend, tokens, sessions and people in the shown time; how much input was
read from cache and how much was spent by subagents; spend over time by
model and by main conversation or subagent; tables of spend by person,
subagent and model; the most expensive sessions; tools with their failures
and duration; subagent runs; refused tool calls; the latest API errors.
The `person` picker narrows everything to one person.

## Sessions

The Sessions page (in the menu while an AI module is switched on) lists
the sessions of the shown time: person, machine, prompts, API calls,
tokens, cost, the share spent by subagents, how long it ran and API
errors. Most recent or most expensive first, for everyone or one person.

A session's page shows its cost split by who spent it (the main
conversation, each subagent, small helper calls such as the session
title) and its prompts in order: what each cost, its API calls, tools and
subagents, and how long it took. Click a prompt for everything that
happened in it, line by line: API calls with model, tokens and cost,
permission decisions, tool calls with their input, replies, finished
subagents and errors. Prompt text, tool input and replies show only when
Claude Code is set to send them (see above). Where traces are kept, a
prompt links to its trace, with subagents nested under the tool call that
started them.

## Not yet

- An audit trail from Claude Code's hooks.
- Comparing with the bill (the Claude Code Analytics API).
