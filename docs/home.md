# Home

The first page after signing in. It says what needs attention now, what
will soon, and what happened since you last looked, without you opening
a dashboard. It reads one answer the server keeps up to date in the
background (`GET /api/v1/briefing`), so it opens at once and puts no load
on the stores; the links lead to the pages that do.

## What it shows

- **Since you last looked:** how many incidents were opened and resolved
  since you last pressed **Mark as seen**. Everyone has their own marker;
  it moves only when you press the button, to what you were shown.
- **Coverage:** how many agents report, which rules cannot be evaluated,
  how many silences are in effect. An empty list means little when half
  the agents are silent, so the page says so.
- **Needs attention:** open incidents, most urgent first: unacknowledged
  before acknowledged, then by severity. Every firing alert is an
  incident (see [Alerts](alerts.md#incidents)). Incidents that belong to
  another are listed under it: everything on a host that is down under the
  host, a HAProxy server under its backend. Silenced incidents are hidden
  behind **Show silenced**. Each line links to where to look next: the
  container's logs, the capture of a detection, the site checks. An admin
  can press **I'm on it**, with a note the others see, or **Explain** (see
  below). When nothing is open the page says so plainly.
- **Coming up:** what will need attention, only where the reason is plain
  and the estimate can be shown:
  - a filesystem that will be **full** within 30 days, as a range ("6 to 9
    days"), with how much it fills a day and a link to the chart. It needs
    three days of data since the last cleanup or resize, the last day and
    the whole week must roughly agree, and filesystems under 1 GB are left
    out. The rate is the median of the slopes between samples, so a burst
    of temporary files barely moves it.
  - **certificates** that expire in 14 to 30 days (within 14 days the
    built-in `CertificateExpiresSoon` alert takes over).
  - **agents held back by their limits** in the last hour; they drop data
    when their spool fills.
  Worked out every ten minutes. "Soon" means within a week.
- **AI spend, last 24 hours:** what was billed against a usual day of the
  week before, subscription use at API prices apart, and how many models
  or GPUs have no price. Shown when anything writes the cost metric (see
  [AI costs](finops.md)).
- **Alerts that keep coming back:** an alert that fired at least five
  times in a week, with how long it usually lasted and a link to its
  history. Whether it needs a longer `for`, a different threshold, a fix
  or a silence is for you to judge; the page suggests none.
- **Resolved in the last 24 hours.**

Clicking an incident shows its history: when it opened, was resolved,
came back, was acknowledged or silenced.

## AI explanations

With a language model set up, an admin can press **Explain** on an
incident (and on a log line in Explore, see
[Explore → A line's context](explore.md#a-lines-context)). vigil gathers the evidence itself, chosen by the kind of
incident; no model decides what to look at:

- the incident, its history and the other open incidents of the host;
- the alert rule behind it; the agent (reporting or not, version, facts);
- configuration changes in the six hours before it started;
- metric summaries of the hour before: for a container its state, exit
  code, restarts, memory and out-of-memory events; for a silent host its
  memory, pressure and the agent's own memory; for a check its answers,
  status and timings; for a HAProxy backend its server states;
- for a runtime detection, the latest matching events **without command
  lines**;
- masked log excerpts from just before the start, only when the server
  allows them (`data: [logs]`).

Before anything is sent the dialog shows exactly what would be: every
item, its class and size, and what was left out and why. **Send and
explain** sends it and shows the answer: what is wrong, the likely cause
with a confidence, the facts it rests on (each citing the evidence items,
which open on a click), what is unknown, and what to check next. A fact
that cites nothing is marked *interpretation*; one that cites evidence
that was never sent is marked in red. Everyone can say whether it was
right, partly right, wrong or unhelpful, with a note; that record is the
only way to know how far to trust these.

Every explanation is stored with the evidence exactly as sent, the model,
the tokens, the cost and the time it took. Whatever was in the bundle was
masked again on the server first: credentials (as the agent masks them)
and e-mail addresses. Captures, command lines and the prompt or tool
content of AI coding tools are never sent.

In the server config:

```yaml
costs:
  tokens:
    - {model: claude-opus-5-5, input: 4.00, output: 20.00, cache_read: 0.20, cache_write: 5.00}
ai:
  provider: anthropic           # the only one so far
  model: claude-opus-5-5        # the default
  effort: low                   # low, medium, high, xhigh, max
  api_key_file: /etc/vigil/anthropic-key
  monthly_budget: 20            # required; in the currency of costs
  data: [logs]                  # optional: also send masked log excerpts
```

(The prices above are an example of the format; check your provider's
current prices.) The model needs a price in `costs.tokens`, because
`monthly_budget` is kept by pricing every call; once it is spent, Explain
refuses until the next month. The key is read from its file on every
call, so a new key needs no restart, and nothing in the server's
environment (`ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, credential
profiles) is used. An admin can **Pause** explanations on the Home page;
nothing is sent while paused. What explanations cost also appears in
[AI costs](finops.md) as the source `vigil`.

Give vigil a key of its own, in a workspace with its own spending limit
at the provider, so a leaked key cannot run up a bill beyond that limit.

## Not here yet

Explanations made on their own when an incident starts, memory and other
trends, grading of security findings, other model providers, and the same
summary as a message to Telegram or Slack. See
`memory/context/direction.md` in the repository for the plan.
