# Fleet: profiles and integrations

With more than a few hosts you do not want to edit a file on each. An
agent's own file then only says who it is; **what it collects is decided on
the server**, from profiles.

## Labels and facts

Profiles select agents by two kinds of attributes.

**Labels** are what you write in the agent's file:

```yaml
labels: {env: prod, role: db}
```

**Facts** are what the agent reports about its host itself:

| Fact | Example |
|---|---|
| `agent` | the agent's id |
| `os`, `arch` | `linux`, `arm64` |
| `kernel` | `6.8.0-45-generic` |
| `distro`, `distro_version` | `ubuntu`, `24.04` |
| `cpus`, `memory_gb` | `8`, `32` |
| `kubernetes` | `true` when running in a cluster |
| `kubernetes_role` | `node` (the DaemonSet) or `cluster` (the cluster agent) |
| `runtime` | `docker`, `containerd`, `crio`, `podman` |
| `ebpf` | `true` when runtime events can work on this kernel |
| `gpu` | `nvidia`, `amd` or both, on hosts whose GPUs the agent can read; absent otherwise |
| `version` | the agent's version |
| `detected_<integration>` | `true` when that technology was found running (see below) |

A label cannot override a fact. Both show on the **Fleet → Agents** page.

## Profiles

A profile is one YAML document:

```yaml
select:                      # all must hold; empty = every agent
  - env="prod"
  - role=~"db|cache"
priority: 20                 # higher is merged later and wins
integrations:                # optional: see below
  postgres: {metrics_target: "localhost:9187"}
config:                      # a fragment of agent configuration
  interval: 15s
  scrape:
    static:
      - {job: pgbouncer, targets: ["localhost:9127"]}
```

Selectors use `=`, `!=`, `=~` and `!~`, as in Prometheus. A regular
expression has to match the whole value: `kernel=~"6\\..*"`, not
`kernel=~"^6"`.

### How profiles combine

An agent's configuration is **every matching profile merged, lowest priority
first**. A common layout:

| Profile | Selects | Priority |
|---|---|---|
| `base` | everything | 0 |
| `prod` | `env="prod"` | 10 |
| `databases` | `role="db"` | 20 |
| `agent-db-07` | `agent="db-07"` | 1000 |

- Sections merge field by field; a later profile's value wins.
- **Scrape jobs, log sources, security rules, probes and process groups
  merge by name.** A later profile can change one field of a job without
  repeating it:

  ```yaml
  config:
    scrape:
      static:
        - {job: node, interval: 15s}       # only the interval changes
        - {job: app, enabled: false}       # switch off what a broader profile set up
  ```

- Other lists (targets, watch paths) are replaced as a whole.
- An agent that no profile matches uses the configuration in its own file.
- The labels in the agent's own file always stay on its data.

### Editing safely

- **Fleet → Profiles → a profile** opens the editor. While you type it shows
  which agents the draft applies to and whether saving would be refused.
- A profile is saved only if **every known agent still gets a valid
  configuration**. Otherwise nothing changes, and the message names an agent
  and the reason.
- Every save is a new **version**. Under History you can compare an old
  version with the editor's text and put it back.
- **Fleet → Agents → Config** shows an agent's merged configuration and, for
  each setting, which profile decided it and which ones it overrode.
  "Override for this agent" starts a profile for just that one.

Agents pick a change up within about 30 seconds, without restarting, and
keep the last configuration on disk in case the server is unreachable.

### Rolling a change out in steps

By default a saved profile reaches all its agents within half a minute. For
profiles where a mistake would hurt, add a `rollout` section, and every
later change to that profile goes to a few agents first:

```yaml
select: ['env="prod"']
rollout:
  steps: [1, 10%, 50%]   # one agent, then 10% of them, then half, then everyone
  wait: 10m              # healthy this long before the next step
config:
  ...
```

- A **step** is a number of agents or a share of the agents the change
  *affects*: those whose merged configuration actually becomes different.
  A share is rounded up and is never less than one agent. After the last
  step comes everyone.
- While the rollout runs, agents in the current step get the new version;
  all others keep the version from before (for a new profile: nothing of
  it). Which agents go first differs from change to change, and only
  agents that are reporting are picked.
- A step is **healthy** when every agent in it has applied the change. With
  `wait`, the next step starts by itself once the step has been healthy
  that long. Without `wait`, each step waits for **Promote**.
- The rollout **pauses by itself** when an agent in it cannot apply the
  change (the reason is shown), or stops reporting after getting it. The
  other agents are untouched.

**Fleet → Profiles** shows each rollout on top: the step, the agents in it
and their state, and when the next step is due. There you can:

| | |
|---|---|
| **Promote** | go to the next step now |
| **Everyone now** | end the rollout; all agents get the new version |
| **Pause** / **Resume** | hold it where it is. Resuming after an agent went silent takes that agent out of the rollout; another takes its place |
| **Abort** | every agent goes back to the earlier version, which is saved again as the newest (a new profile is deleted) |

On the Agents tab, a profile an agent has ahead of the others is marked
"new".

Things to know:

- Saving the profile again during its rollout (a fix) continues with the
  same agents and step; the others still stay on the version from before
  the rollout began.
- "Apply to every agent at once" in the editor, `?rollout=skip` on the API
  and `vigil apply -immediate` skip the steps for one save. For emergencies.
- A change that affects no agent finishes at once. So does one where all
  affected agents are already in an early step.
- Deleting a profile is not staged.
- Health here means "the agent took the config and keeps reporting". It
  does not look at alerts or at what the change does to the host; use
  `wait` long enough for your alerts to speak, and Pause or Abort if they
  do.
- After **Abort**, files in git still hold the new text: revert it there
  too, or the next `vigil apply` starts the rollout again.

## Integrations

An integration bundles what one technology needs: how to recognise it, what
to collect, alert rules and a dashboard. Built in: Docker, Kubernetes node,
Kubernetes cluster, GPUs, HAProxy, nginx, PostgreSQL, Redis, VictoriaMetrics,
SSH logins, and Boop, Boop Jump gateway and Boop Jump agent (see
[Boop and Boop Jump](boop.md)).

A profile switches one on, with parameters where the defaults do not fit:

```yaml
select: ['role="db"']
integrations:
  postgres: {metrics_target: "localhost:9187", log_path: /srv/pg/log/*.log}
config:
  scrape:
    static:
      - {job: postgres, interval: 10s}    # adjust what the integration set up
```

While some profile uses an integration, its alert rules run (named
`<integration>/<group>` on the Alerts page) and its dashboard is installed.
An alert of the same name in the rule file runs instead of the
integration's (see [Alerts](alerts.md)).
The dashboard is yours to change or delete; what happens to it when a
newer vigil brings a newer version is described under
[Dashboards](dashboards.md#dashboards-that-come-with-vigil).

Parameters reach the alert rules as well as the agents' config. The rules
run once for the whole server, so when several profiles switch an
integration on with different values, the profile with the highest
priority decides; parameters it does not give keep their defaults.

nginx, PostgreSQL and Redis metrics come from their usual Prometheus
exporters, which must run next to the service. The integration's parameters
say where each is expected.

Where container logs are collected, the HAProxy integration also reads
HAProxy's access log (`option httplog`) from containers named or built
from `haproxy*` (parameter `log_container`): backend, server, status,
timings and path become fields, and the lines carry `app="haproxy"`.

The SSH logins integration (`sshd`) reads sshd's lines from the journal
by sshd's own unit and takes `Accepted` and `Failed` lines apart: user,
method, source address, key type, and for a certificate its Key ID, serial
and CA (`ssh_user`, `ssh_method`, `ssh_from`, `ssh_key_type`, `ssh_key_id`,
`ssh_cert_serial`, `ssh_ca_fp`, …). It is not detected by itself, since
sshd runs nearly everywhere; switch it on where you want it.

HAProxy needs no exporter: it has one built in (2.0 and newer). Give it a
frontend for it, reachable by the agent only, and point `metrics_target`
at that:

```
frontend stats
    bind 127.0.0.1:8404
    http-request use-service prometheus-exporter if { path /metrics }
```

### Detection

Agents report the names of running processes, listening TCP ports and
container images (names only, never command lines), when they change. The
server matches this against the integrations:

- **Fleet → Integrations** shows for each: found on how many agents,
  switched on for how many, and where it **runs unmonitored**.
- **Enable where detected** writes a profile that follows the detection:

  ```yaml
  select: ['detected_postgres="true"']
  integrations: {postgres: {}}
  ```

  Any host PostgreSQL shows up on later is covered without anyone touching
  the profile.

### Your own integrations

**Fleet → Integrations → New integration**, or open a built-in one and save
a changed copy. The document:

```yaml
title: Shop API
description: Our order service.
detect:
  process: [shop-api]           # or image: [shop-api]
params:
  target: {default: "localhost:8080", description: Where its metrics are.}
config:
  scrape:
    static:
      - {job: shop, targets: ["${target}"], path: /actuator/prometheus}
rules:                          # optional, in the rule-file format
  - name: alerts
    rules:
      - alert: ShopDown
        expr: up{job="shop"} == 0
        for: 2m
dashboard: {...}                # optional, a dashboard as JSON or YAML
queries:                        # optional, offered in Explore under Saved
  - name: Orders per minute
    query: sum(rate(shop_orders_total[1m])) * 60
  - name: Shop errors
    type: logs                  # metrics (PromQL) is the default
    query: app:shop level:error
```

The queries show up in Explore, for everyone, while a profile switches the
integration on. They cannot be changed there, only copied.

## Keeping profiles in git

Profiles and integrations can live as files and be applied from CI:

```
fleet/
  base.yaml
  prod.yaml
  databases.yaml
  integrations/
    shop.yaml
  queries/
    team.yaml
```

```bash
export VIGIL_SERVER=https://vigil.example.com VIGIL_ADMIN_TOKEN=…
vigil apply -f fleet/ -dry-run     # show what would change
vigil apply -f fleet/ -prune       # apply; delete file-managed profiles no longer present
```

It changes only what differs. `-prune` deletes only profiles that were
applied from files before; profiles made in the interface are left alone.
Both ways of editing can be mixed; each profile shows where it was last
written from. `configs/profiles/` in the repository has examples.

Files under `queries/` hold [saved queries](explore.md#saved-queries) that
everyone gets in Explore, as a list under `queries:` in the form shown for
integrations above. A query is known by its name within its type, across
all the files. A shared query of that name made in the interface is taken
over. `-prune` deletes shared queries that came from files and are in none
any more; personal queries and shared ones made in the interface are never
deleted. Without a `queries` directory the saved queries are left alone
altogether.

A profile with a `rollout` section is rolled out in steps when applied
this way too; `vigil apply` says so and returns without waiting for it.

If the server uses its own certificate authority, give `vigil apply` the
fingerprint: `-ca-pin sha256:…` or `VIGIL_CA_PIN`.

## Tokens and forgetting agents

- **Enrolment** creates a token many hosts can use to get a token of their
  own; see [Install](install.md#many-hosts-at-once). Revoking it stops new
  hosts from joining and leaves enrolled ones alone.
- **Add agent** creates a token that only that agent id can use.
- **Manage** on an agent replaces or revokes its token, or forgets the
  agent. Its stored metrics and logs are kept.
