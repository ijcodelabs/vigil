# vigil

vigil is one monitoring system for metrics, logs, alerts, runtime security
events and dashboards. It is a few static Go binaries: the **agent**
(`vigil-agent`) on every host or Kubernetes node and the **server**
(`vigil-server`) in the middle.

```
 hosts / Kubernetes nodes                      server
┌───────────────────────┐              ┌────────────────────────────┐
│ vigil-agent           │   push  ──▶  │ vigil-server               │
│  metrics, logs,       │              │  checks tokens, stores,    │
│  runtime events       │  ◀── config  │  evaluates rules, alerts,  │
│  queue on disk        │              │  serves the web interface  │
└───────────────────────┘              └───────┬──────────┬─────────┘
                                               ▼          ▼
                                       VictoriaMetrics  VictoriaLogs
                                       (and VictoriaTraces, if you keep traces)
```

- Agents collect and **push** to the server; whatever they could not send
  waits in a queue on disk. The server can also **pull** from hosts that run
  no agent.
- The server keeps metrics in VictoriaMetrics, logs in VictoriaLogs and,
  optionally, traces in VictoriaTraces. Nothing else talks to them.
- The **Home** page is the first one after signing in: what needs
  attention now, what will soon, and what happened since you last looked.
- What agents collect is managed on the server, in **profiles** selected by
  labels, so a host's own file only says who it is and where the server is.

## Where to start

| I want to… | Read |
|---|---|
| get a server and a first agent running | [Install](install.md) |
| have an AI assistant install it for me | [Install with an AI assistant](install-by-ai.md) |
| decide what an agent collects | [Agent configuration](configs.md) |
| manage many agents | [Fleet: profiles and integrations](fleet.md) |
| monitor Kubernetes | [Kubernetes](kubernetes.md) |
| reach the server from a site that cannot connect to it | [Relays](relay.md) |
| look at data | [Explore](explore.md), [Dashboards](dashboards.md), or your own [Grafana](grafana.md) |
| see how your applications do: requests, errors, latency, traces | [Services and traces](apm.md) |
| watch GPUs and what AI use costs | [GPUs and AI machines](gpu.md), [AI costs](finops.md) |
| see what Claude Code costs and does | [Claude Code](claude-code.md) |
| watch Boop and Boop Jump, and SSH logins that bypass them | [Boop and Boop Jump](boop.md) |
| see at a glance what needs attention | [Home](home.md) |
| keep the AI key in your OpenBao or Vault | [Secrets](secrets.md) |
| be told when something is wrong | [Alerts](alerts.md) |
| set and track objectives: availability, latency, error budgets | [Service level objectives](slo.md) |
| see what processes do, and catch intrusions | [Runtime events](security.md) |
| know what a metric means | [Metric reference](metrics.md) |
| script against it | [API](api.md) |
| find out why something does not work | [Troubleshooting](troubleshooting.md) |

## Words used here

- **Agent**: `vigil-agent`, running on a host. It has an id (its host name
  by default).
- **Labels**: what you say a host is, in its own file: `env: prod`,
  `role: db`. Added to everything it sends.
- **Facts**: what an agent finds out about its host itself: `os`, `kernel`,
  `kubernetes`, and so on.
- **Profile**: a piece of agent configuration on the server, with a selector
  saying which agents it applies to.
- **Integration**: everything needed to monitor one technology (nginx,
  PostgreSQL, Kubernetes, …): what to collect, alert rules, a dashboard.
- **SLO**: a service level objective, the share of events that has to be
  good over a window; what is left to lose is the error budget.
- **Incident**: one thing that is wrong, made from a firing alert, with a
  history (opened, acknowledged, resolved, reopened).
- **Detection**: a security rule that matched something a process did.
- **Capture**: the recorded activity of a host around a detection.
