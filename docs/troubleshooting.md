# Troubleshooting

Start with the agent's own log (`journalctl -u vigil-agent`, or
`kubectl -n vigil logs ds/vigil-agent`) and the **Fleet** page. Most
problems say what they are in one of the two.

## An agent does not appear on the Fleet page

- **Is it running?** `systemctl status vigil-agent`.
- **Can it reach the server?** The log says `push failed, will retry` with
  the reason. Check the URL, DNS and firewalls.
- **`HTTP 401`**: the token is wrong. **`HTTP 403 … different agent`**: the
  token was issued for another agent id; `agent_id` must match.
- **`HTTP 503: relay … cannot reach the server`**: the agent reaches its
  relay, the relay does not reach the server. Look at the relay's log; the
  agent keeps its data meanwhile. **`HTTP 404: this is a vigil relay`**:
  something other than an agent is pointed at a relay; the interface and
  the API are on the server.
- An agent shows up after its first successful push, so within about a
  minute of starting.
- **`cannot enrol with the server`**: the reason follows. *Not known*: the
  enrolment token was revoked or mistyped. *Expired* or *used as often as it
  allows*: make a new one. *Already enrolled*: that agent id has a token;
  revoke it on the Fleet page (Manage), or set another `agent_id`. The agent
  keeps trying, once a minute after a refusal.
- **`certificate authority has fingerprint … not the expected …`**: `ca_pin`
  is for another server, or the server's `data_dir/pki` was replaced. The
  current fingerprint is under Fleet → Enrolment and in the server's log.
- **`x509: certificate is valid for …, not <address>`**: the server runs in
  a container and agents connect to an address of the host it cannot see.
  Add that address to `tls.names` in the server's config.
- **`x509: certificate signed by unknown authority`**: the server uses its
  own authority and the agent has no `ca_pin` (or `ca_file`).
- **`the server now expects https`**: the server was switched to TLS; change
  `server.url` in the agent's file. Nothing is lost meanwhile.
- **`HTTP 401` after a re-install**: the host still has an old
  `spool_dir/agent-token`. Delete it so the agent enrols again.

## An agent is "over limit"

It sends more than the server's `limits` allow, so the server takes only
that much and the agent queues, then drops, the rest
(`vigil_agent_dropped_total{reason="spool_full"}`). Find out what grew:
`topk(10, count by (job) ({agent="NAME"}))` for series,
`_time:1h agent:NAME | stats by (source, container) count()` for logs. Then
either make the agent collect less (a `drop` on the job or the log
template, the agent's own `limits`), or raise the server's limit for that
agent under `limits.agents`. See [Install](install.md#limits).

## It appears but there is no data in Explore

- **Wait 30 seconds.** The metric store hides the most recent half minute.
- Check the time range, and that the clock on the host is right. Data
  stamped in the future is not shown until that time arrives.
- `vigil_agent_spool_bytes` growing means the agent cannot deliver; its log
  says why.

## The server answers 503 to agents

`metrics store unavailable` or `log store unavailable`: the server cannot
reach VictoriaMetrics or VictoriaLogs. Agents keep the data on disk and
deliver it when the store is back; nothing is lost until `spool_max_bytes`
is reached.

## A config change does not take effect

- The **Config** column on the Fleet page:
  - **pending**: the agent has not fetched it yet (it checks about every 30
    seconds), or it is not running.
  - **error**: the reason is shown under it.
- **Config** on the agent shows which profiles apply. If the one you expect
  is missing, its selector does not match: compare it with the agent's
  labels and facts on that page. A regular expression must match the whole
  value.
- `remote_config: false` in the agent's file makes it ignore the server.

## A rollout does not move

- **Waiting for you**: the profile's `rollout` has no `wait`, so each step
  needs Promote.
- **Paused**: the reason is on the Profiles tab. *Could not apply*: fix the
  profile and save (the rollout continues), or Abort. *Stopped reporting*:
  look at that host first; Resume replaces it with another agent.
- **"Waiting for more agents to report"**: fewer agents are reporting than
  the step asks for. It continues with those there are.
- **Some agents still have the old settings**: that is the rollout; the
  Agents tab marks who has the new version.
- After a server restart a step's waiting time starts again.

## A profile or rule file is refused

The message says why. Common ones:

- `field … not found`: a typo in a setting's name.
- `agent X would get an invalid config`: with this change the merged result
  for that agent is wrong, for example a log source naming a template that
  no profile defines, or a job left without targets because the profile
  that defined it was removed.
- Rules: `query refused` is a syntax error in the expression, quoted from
  the store. `rules not saved, their expressions could not be checked`
  means a store is unreachable.

## Logs are missing

- **A file that existed before the agent first saw it is read from its
  end.** Use `from_beginning: true` for what is already there.
- The agent needs permission to read the file; it runs as root in the
  provided unit.
- `journald logs disabled: journalctl not found`: journald needs a host
  install, not a container.
- `cannot list docker containers`: the socket path is wrong or not mounted.
- Containers started with `vigil.logs=false`, or pods annotated
  `vigil.io/logs: "false"`, are skipped on purpose.
- `vigil_agent_dropped_total{reason="log_rate_limit"}` above zero: the host
  logs more than `limits.max_log_bytes_per_second`.

## Lines are not parsed

- `pod asks for an unknown log template` / `container asks for an unknown
  log template`: the name in the annotation or label is not defined in the
  agent's configuration.
- A line a `regex` template does not match is kept whole, unparsed. Test the
  pattern against a real line.
- Fields are searched as `field:value`; if `level:error` finds nothing but
  `error` does, the template did not extract `level`.
- **A stack trace from a systemd service arrives as many entries**: give
  that unit a template with `multiline.start` under
  `logs.journald.unit_templates`.
- **A container's lines have no fields**: check which template it gets.
  Its own label wins, then `docker.container_templates` (matched on the
  container's and the image's name), then `docker.template`. A changed
  template applies to new lines only.

## Metrics are missing

- `up == 0` for the job: the agent's log has the scrape error.
- `vigil_agent_dropped_total{reason="series_limit"}` above zero: the host
  sends more distinct series than `limits.max_series`. Usually one label
  with unbounded values; find it, or `drop` the metric.
- `rate()` returns nothing: the window must be at least twice the
  collection interval.

## Alerts do not notify anyone

- **Alerts → Contacts & routing**: if the only contact is `none`, nobody is
  configured.
- **Alerts → Rules**: the rule must be *firing*, not *pending* (still inside
  its `for` time), and not in error.
- **Alerts → Silences**: is a silence matching it?
- Alertmanager waits `group_wait` before the first message and
  `repeat_interval` between reminders.
- Delivery failures (wrong SMTP password, unreachable webhook) are in the
  server's log.

## Runtime events are off

The Fleet page shows the reason next to the agent:

- `not permitted to load eBPF programs`: the agent is not root, or its
  container is not privileged.
- `this kernel has no BTF` or `Linux 5.8 or newer is needed`: the kernel
  cannot run it. Select only capable hosts with `ebpf="true"`.
- A rule does not fire: run `sudo vigil events -config …` on the host, do
  the thing, and compare the event's fields with the rule. File opens are
  only seen for absolute paths under `watch_paths`.

## Kubernetes

- `kubelet: HTTP 403`: the service account lacks `get` on `nodes/metrics`;
  re-apply `deploy/k8s-agent.yaml`.
- `kubelet metrics are off … NODE_IP is not set`: the manifest is older than
  the agent; re-apply it.
- The agent's pod stays in `CreateContainerError` with `not a shared or
  slave mount`: remove `mountPropagation` from the host root mount (current
  manifests do not have it).
- Cluster agent: `list … HTTP 403`: apply `deploy/k8s-cluster-agent.yaml`
  again for its cluster role.
- Everything appears twice: more than one cluster agent is running.

## The interface

- **Signed out unexpectedly**: sessions last seven days; creating the first
  user account ends the admin-token sign-in on purpose.
- **Too many failed sign-ins**: wait five minutes.
- A dashboard panel says `This panel could not be drawn`: its definition is
  wrong; edit the panel or the dashboard's JSON.
