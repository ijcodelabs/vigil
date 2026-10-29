# Installing vigil with an AI assistant

This page is written for an AI coding assistant (Claude Code or similar)
that a person has asked to do one of these:

- "Install the vigil server and agents on my infrastructure."
- "Install vigil agents on these servers and connect them to my vigil
  server."

If you are a person: give your assistant this file (or the repository) and
one of those sentences. If you are the assistant: everything you need is
below. Follow it in order, and do not skip the verification after each
stage. The other pages in `docs/` have the detail; this one is the route.

## Rules for the assistant

1. **Ask before you assume.** Collect the answers under
   [What to find out first](#what-to-find-out-first) before changing
   anything. Do not guess host names, addresses or which hosts are in scope.
2. **Look before you change.** On every host, check what is there first
   (`ls /etc/vigil /opt/vigil 2>/dev/null`, `systemctl status vigil-agent`,
   `docker ps`, listening ports). Never overwrite an existing
   `/etc/vigil/agent.yaml`, `server.yaml` or data directory without asking.
3. **Leave other monitoring alone.** Prometheus, Grafana, Alloy, node
   exporters and the like stay as they are unless the person asks to remove
   them. vigil runs beside them.
4. **Secrets.** Generate tokens on the target host (`openssl rand -hex 32`),
   keep them in files with mode 600, and do not print them into the
   conversation. Tokens shown by the server are shown once: write them
   straight to where they belong.
5. **Ask before changes outside vigil**: firewall rules, reverse proxies,
   DNS, cloud security groups, anything that restarts a service other
   people use. Say what you want to change and why, and wait.
6. **Never serve vigil unencrypted across a network you do not control.**
   Use `tls.self_signed: true`, your own certificate, or a TLS-terminating
   proxy. Plain HTTP is for a first look on one machine only.
7. **Verify, then report.** After each stage run its checks. If a check
   fails, stop and use [When something fails](#when-something-fails); do not
   pile further steps on a broken one. At the end, tell the person what was
   installed where, what was not done, and how to undo it.
8. **One host first.** Install one agent, see it report, then do the rest.

## What vigil is, in facts you can rely on

- **One static binary**, `vigil`, with sub-commands: `server`, `agent`,
  `relay`, `enroll`, `apply`, `events`, `tracegen`, `version`. Linux amd64
  and arm64. No packages, no runtime dependencies.
- **Server**: web interface and API on one port (default `:8080`). It keeps
  its own state in `data_dir` (SQLite, certificates, captures) and needs
  three stores it alone talks to:

  | Store | Holds | Default port | Needed |
  |---|---|---|---|
  | VictoriaMetrics | metrics | 8428 | yes |
  | VictoriaLogs | logs | 9428 | yes |
  | VictoriaTraces | traces | 10428 | only for traces |

- **Agent**: one per host (or per Kubernetes node). It *pushes* to the
  server over HTTP(S); the server never connects to agents. So agents need
  an outbound route to the server's port and nothing inbound.
- **What an agent collects is decided on the server**, in *profiles* that
  select agents by labels. The agent's own file only needs: where the
  server is, a token, and labels saying what the host is.
- **Tokens**: `admin_token` (server config; API and first sign-in),
  enrolment tokens (`vge_…`, exchanged once by each host for a token of its
  own), agent tokens (`vgl_…`, valid for one agent id).
- **Where things live on a host**:

  | | Path |
  |---|---|
  | binary | `/usr/local/bin/vigil-agent` (server: `vigil-server`) |
  | agent config | `/etc/vigil/agent.yaml` |
  | agent state (unsent data, positions, its token if self-enrolled) | `/var/lib/vigil/spool` |
  | server config | `/etc/vigil/server.yaml` (or next to the compose file) |
  | server state | `data_dir`, default `/var/lib/vigil` |

- **There are no published packages or images yet.** Build from the
  repository: `make linux` gives `bin/vigil-agent-linux-amd64`,
  `bin/vigil-server-linux-amd64`, `bin/vigil-relay-linux-amd64` and the
  same for arm64 (needs Go 1.26+; nothing else), and
  `docker build --target agent -t vigil-agent:dev .` gives the agent's
  container image (`--target server`, `--target relay` for the others).

## What to find out first

Ask the person; do not proceed on guesses.

**Always**

- Which hosts are in scope, how you reach them (SSH alias or user and
  host), and whether you have root or sudo there.
- For each host, or group of hosts: what it is. This becomes labels, for
  example `env=prod`, `role=web`. Profiles select by these.

**If you install the server**

- Which host runs it, and whether Docker or Podman is available there
  (compose is the simplest route).
- By which address agents will reach it (a DNS name or an address, and
  whether that is a private network or VPN).
- How people will reach the interface: same address, or a public name
  behind an existing reverse proxy.
- How long to keep data (defaults in the examples: metrics 30 days, logs 14
  days, traces 7 days) and roughly how much disk that host has.

**If you connect agents to an existing server**

- **By which address the hosts reach the server.** It is often not the
  address people use for the interface: a server behind a reverse proxy may
  refuse what agents send on its public name, and offer agents a private or
  VPN address instead. Test it from each host (B1) before anything else.

- The server's address (for example `https://vigil.example.com:8080`).
- An **enrolment token**, or an admin token with which you may create one.
  If the person has neither, they create one in the interface under
  **Fleet → Enrolment → New enrolment token**; that page also shows the
  certificate fingerprint and the exact command.
- Whether the server uses its own certificate authority. If so you need its
  fingerprint (`ca_pin`, `sha256:…`). It is on that same page, in the
  server's start-up log, and at `/api/v1/server-info` for a signed-in user.

**Optional, ask once**

- Kubernetes clusters to include? Applications instrumented with
  OpenTelemetry? GPUs? Should alerts notify someone (email, Slack,
  Telegram, webhook)?

## Route A: install the server

### A1. Build

On a machine with Go 1.26+ and the repository:

```bash
make linux                      # bin/vigil-server-linux-amd64, bin/vigil-agent-linux-amd64, ... and -arm64
```

Pick the binary for the server host's architecture (`uname -m`: `x86_64` is
amd64, `aarch64` is arm64).

### A2. Lay it out on the server host

Use one directory, for example `/opt/vigil`. Copy the server binary
(`vigil-server-linux-amd64` or `-arm64`) there as `vigil-server`, and
create these three files.

`Dockerfile`:

```dockerfile
FROM alpine:3.22
COPY vigil-server /vigil-server
ENTRYPOINT ["/vigil-server"]
```

`docker-compose.yml` (adjust retention; publish the port only on the
address agents and people should reach, `8080:8080` for all addresses):

```yaml
name: vigil
services:
  vigil:
    build: .
    image: vigil:local
    command: ["-config", "/etc/vigil/server.yaml"]
    env_file: .env
    volumes:
      - ./server.yaml:/etc/vigil/server.yaml:ro
      - vigil:/var/lib/vigil
    ports: ["8080:8080"]
    depends_on: [victoriametrics, victorialogs, victoriatraces]
    restart: unless-stopped
  victoriametrics:
    image: docker.io/victoriametrics/victoria-metrics:v1.153.0
    command: ["-storageDataPath=/storage", "-retentionPeriod=30d"]
    volumes: ["vm:/storage"]
    restart: unless-stopped
  victorialogs:
    image: docker.io/victoriametrics/victoria-logs:v1.53.0
    command: ["-storageDataPath=/vlogs", "-retentionPeriod=14d"]
    volumes: ["vlogs:/vlogs"]
    restart: unless-stopped
  victoriatraces:
    image: docker.io/victoriametrics/victoria-traces:v0.12.0
    command: ["-storageDataPath=/vtraces", "-retentionPeriod=7d"]
    volumes: ["vtraces:/vtraces"]
    restart: unless-stopped
volumes: {vigil: {}, vm: {}, vlogs: {}, vtraces: {}}
```

`server.yaml` (replace the address with the one agents and people use):

```yaml
listen: ":8080"
data_dir: /var/lib/vigil
external_url: https://vigil.example.com:8080
tls:
  self_signed: true          # HTTPS with the server's own certificate authority
  # names: ["10.0.0.5"]      # see the note below
storage:
  metrics_url: http://victoriametrics:8428
  logs_url: http://victorialogs:9428
  traces_url: http://victoriatraces:10428
admin_token: ${VIGIL_ADMIN_TOKEN}
```

**Certificate names.** With `self_signed`, the certificate covers the host
in `external_url` and whatever name clients ask for. One thing it cannot
learn by itself: in a container with a published port, the *host's address*
that agents connect to. If agents will use an address (or name) that is not
the host in `external_url`, list it under `tls.names`.

The admin token, generated in place and never shown:

```bash
cd /opt/vigil && umask 077 && echo "VIGIL_ADMIN_TOKEN=$(openssl rand -hex 32)" > .env
```

Other ways to encrypt (`tls.cert_file` + `tls.key_file` for a certificate
you have, or a reverse proxy with `trusted_proxies`) are in
[Install → Encryption](install.md#encryption). Without containers, run the
stores as their own services and `vigil-server -config …` under systemd;
see [Install → Without containers](install.md#without-containers).

### A3. Start and verify

```bash
cd /opt/vigil && docker compose up -d --build
```

Checks, all must pass:

```bash
curl -sk https://127.0.0.1:8080/healthz                     # prints: ok
docker compose ps                                            # four containers, all Up
docker compose logs vigil | grep -i "listening"              # "encrypted with its own certificate authority", with ca_pin=sha256:…
docker compose logs vigil | grep -ci "level=ERROR"           # 0
```

Read the fingerprint and keep it for the agents (it is not a secret):

```bash
. ./.env
curl -sk -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://127.0.0.1:8080/api/v1/server-info
# {"ca_pin":"sha256:…","encrypted":true,"traces":true,"url":"…","version":"…"}
```

### A4. Hand the interface to the person

Tell them: open `external_url`, sign in as `admin` with the admin token as
the password (it is in `.env` on the server; do not paste it into the
conversation), then create their own account under **Users**. After that
the token no longer signs in to the interface and remains for scripts. The
browser warns once about the certificate, because the server is its own
authority; `data_dir/pki/ca.pem` can be imported to stop that.

Then continue with Route B for the agents. The server's own host should get
an agent too.

## Route B: install agents and connect them to a server

You need: the server address, and either an enrolment token or an admin
token to create one.

### B1. Check the server from where you are

```bash
curl -sk https://SERVER:8080/healthz          # ok
```

`/healthz` answering is not enough when a reverse proxy is in front: check
that the proxy lets agent requests through as well. This must answer `401`
(the server asking for a token), not `404` or `403` from the proxy:

```bash
curl -sk -o /dev/null -w '%{http_code}\n' -X POST https://SERVER:8080/api/v1/write
```

If either check fails from a host that should become an agent, stop: fix
the route (DNS, firewall, VPN, proxy rule) with the person before
installing anything. If hosts
cannot reach the server at all but one machine in their network can, that
machine can run a relay; see [Relays](relay.md).

### B2. Get an enrolment token (if you were not given one)

With the admin token in the environment, not on the command line:

```bash
curl -sk -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"installed by assistant","valid_hours":24,"max_uses":0}' \
  https://SERVER:8080/api/v1/fleet/enrollment
# {"id":"…","name":"…","token":"vge_…","expires":"…"}   <- the token is shown only this once
```

`max_uses: 0` means any number of hosts; give a number if you know it. Keep
the token in a variable or a mode-600 file, not in the conversation.

The fingerprint, if the server has its own authority:

```bash
curl -sk -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://SERVER:8080/api/v1/server-info   # "ca_pin"
```

### B3. A Linux host (the normal case)

For each host. Run as root.

```bash
# 1. the binary for this host's architecture
install -m 755 vigil-agent-linux-amd64 /usr/local/bin/vigil-agent     # or -arm64; copy it there first (scp)
vigil-agent version

# 2. enrol: exchanges the enrolment token for this host's own token and writes /etc/vigil/agent.yaml
vigil-agent enroll -server https://SERVER:8080 -token "$ENROLL_TOKEN" -ca-pin sha256:… \
  -label env=prod -label role=web
#    -id NAME     if the agent should not be called by the host name
#    leave -ca-pin out if the server has a certificate from a public authority

# 3. the service
cat > /etc/systemd/system/vigil-agent.service <<'EOF'
[Unit]
Description=vigil monitoring agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/vigil-agent -config /etc/vigil/agent.yaml
EnvironmentFile=-/etc/vigil/agent.env
StateDirectory=vigil
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now vigil-agent
```

`vigil-agent enroll` refuses to run if `/etc/vigil/agent.yaml` exists: that host
is already set up. Ask before replacing it. It also refuses a name that is
already enrolled; the person revokes that agent's token on the Fleet page
(Manage), or you choose another `-id`.

The agent runs as root in this unit. It needs that for all log files, other
users' processes and runtime events; metrics alone work without.

Checks on the host:

```bash
systemctl is-active vigil-agent                                   # active
journalctl -u vigil-agent -n 20 --no-pager | grep -c "level=ERROR"  # 0
journalctl -u vigil-agent -n 20 --no-pager | grep "push failed"   # nothing, after the first seconds
```

Check on the server, about a minute later:

```bash
curl -sk -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://SERVER:8080/api/v1/agents
# the host's entry has "up":true
```

### B3b. A NixOS host

NixOS manages `/etc/systemd` itself, so step 3 of B3 does not apply: a unit
file copied there is gone at the next rebuild, and `/usr/local/bin` does not
exist. The static binary runs on NixOS as it is. Do this instead:

1. Copy the binary to `/opt/vigil/vigil-agent` (mode 755) and run `vigil-agent enroll`
   from there, as in B3 step 2. `/etc/vigil/agent.yaml` is an ordinary file
   and stays.
2. Add `deploy/vigil-agent.nix` from the repository to the host's
   configuration (where the person keeps it: ask, it is usually a git
   repository) and switch it on:

   ```nix
   imports = [ ./vigil-agent.nix ];
   services.vigil-agent.enable = true;
   ```

3. `nixos-rebuild switch`, by the person's usual workflow (commit, pull,
   switch), then the same checks as in B3.

The module puts `journalctl` and `systemctl` on the unit's PATH, which the
agent needs for journald logs and unit states.

### B4. Many hosts

Do B3 on one host first. Then either repeat it per host, or, for
provisioning tools and images, ship the same file everywhere and let each
agent enrol itself on first start:

```yaml
# /etc/vigil/agent.yaml, identical on every host
server:
  url: https://SERVER:8080
  enroll_token: ${VIGIL_ENROLL_TOKEN}     # from /etc/vigil/agent.env, mode 600
  ca_pin: sha256:…
labels:
  env: prod
```

The agent enrols under its host name and keeps its own token in
`/var/lib/vigil/spool/agent-token`.

### B5. A host that only runs containers

Run the agent as a container that sees the host. It needs an image
(`docker build --target agent -t vigil-agent:dev .` in the repository, or
the three-line Dockerfile above with `vigil-agent` in place of
`vigil-server`) and an `/etc/vigil/agent.yaml` on the host, written as in
B4:

```bash
docker run -d --name vigil-agent --restart unless-stopped --pid host --network host \
  -e HOST_PROC=/host/proc -e HOST_SYS=/host/sys -e HOST_ETC=/host/etc -e HOST_ROOT=/host/root \
  -v /proc:/host/proc:ro -v /sys:/host/sys:ro -v /etc:/host/etc:ro -v /:/host/root:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/vigil:/etc/vigil:ro -v /var/lib/vigil:/var/lib/vigil \
  vigil-agent:dev -config /etc/vigil/agent.yaml
```

Prefer the host install (B3) where you can: in a container there is no
journald and no systemd unit state.

### B6. Kubernetes

Two manifests in `deploy/`: `k8s-agent.yaml` (a DaemonSet: every node, its
pods and their logs) and `k8s-cluster-agent.yaml` (one Deployment: the
cluster's objects and events). In both, set `server.url` and replace
`my-cluster` with the cluster's name; set `image:` to where you pushed the
image. Then:

```bash
kubectl create namespace vigil
kubectl -n vigil create secret generic vigil-agent \
  --from-literal=enroll-token="$ENROLL_TOKEN" --from-literal=ca-pin=sha256:…
kubectl apply -f deploy/k8s-agent.yaml
kubectl -n vigil rollout status ds/vigil-agent
```

The cluster agent cannot enrol itself (its pod keeps no disk). Create a
token for its `agent_id` and add it to the secret, then apply it:

```bash
curl -sk -X POST -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://SERVER:8080/api/v1/agents/CLUSTER-state/token
#   -> {"agent_id":"…","token":"vgl_…"}
kubectl -n vigil patch secret vigil-agent -p '{"stringData":{"cluster-token":"vgl_…"}}'
kubectl apply -f deploy/k8s-cluster-agent.yaml
```

Details and what is collected: [Kubernetes](kubernetes.md).

## Route C: decide what agents collect

Without any profile, an agent uses its own file, which after `vigil-agent enroll`
says nothing about collection, so it sends host metrics and little else.
Give the fleet profiles. Keep them as files in the person's repository and
apply them; that way the configuration is reviewable and repeatable.

A sensible start, as two files in a directory `fleet/`:

```yaml
# fleet/base.yaml  — every agent
priority: 0
config:
  interval: 15s
  host_metrics: true
  logs:
    journald: {enabled: true}
  systemd: {enabled: true}
  limits: {max_series: 50000, max_log_bytes_per_second: 1048576}
```

```yaml
# fleet/docker.yaml  — hosts where a container runtime was found
select: ['runtime=~"docker|podman"']
priority: 10
integrations:
  docker: {}
```

Apply (from any machine that reaches the server):

```bash
export VIGIL_SERVER=https://SERVER:8080 VIGIL_ADMIN_TOKEN=… VIGIL_CA_PIN=sha256:…
vigil apply -f fleet/ -dry-run     # what would change
vigil apply -f fleet/              # do it
```

The server refuses a profile that would give any known agent an invalid
configuration, and says which agent and why; fix the file, do not force it.

Further, as the person's infrastructure calls for:

- **Integrations** bundle collection, alert rules and a dashboard for one
  technology: `docker`, `kubernetes`, `kubernetes_cluster`, `haproxy`,
  `nginx`, `postgres`, `redis`, `victoriametrics`, `gpu`, `sshd`, `boop`,
  `boop_jump_gateway`, `boop_jump_agent`. The Fleet page's
  Integrations tab shows what was *detected* on which host and not yet
  switched on. Some need an exporter running next to the service (nginx,
  PostgreSQL, Redis); say so rather than install exporters unasked.
  [Fleet](fleet.md#integrations)
- **Scrape targets** the person already has (anything that serves
  Prometheus metrics): `scrape.static` in a profile.
  [Agent configuration](configs.md)
- **Noisy logs**: a log template with `drop` patterns; health-check lines
  are the usual culprit. [Agent configuration](configs.md)
- **Applications with OpenTelemetry**: `otlp: {enabled: true}` and
  [Services and traces](apm.md).
- **Who is told about alerts**: [Alerts](alerts.md#who-is-told-contacts-and-routing).
  A new server notifies nobody until a receiver is configured.
- **Careful changes on large fleets**: a `rollout` section in a profile.
  [Fleet](fleet.md#rolling-a-change-out-in-steps)

Leave **runtime events** (`security.enabled`) off unless asked: they need
Linux 5.8+, root, and rules written for the host.
[Runtime events](security.md)

## Final verification

All of these, before you report success:

```bash
A() { curl -sk -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" "$@"; }

# every agent you installed is up and has its configuration
A https://SERVER:8080/api/v1/agents
#   per agent: "up":true and "config_state":"in sync"  (not "error"; "pending" for under a minute is normal)

# metrics arrive (wait 40 seconds after the first start: the newest half minute is not shown)
A --data-urlencode 'query=count by (agent) (vigil_agent_up == 1)' https://SERVER:8080/api/v1/query

# logs arrive
A --data-urlencode 'query=_time:5m | stats by (agent, source) count() lines' https://SERVER:8080/api/v1/logs/query

# nothing is being dropped
A --data-urlencode 'query=sum by (agent, reason) (vigil_agent_dropped_total) > 0' https://SERVER:8080/api/v1/query
#   empty result is what you want
```

Then tell the person:

- the interface's address and how to sign in the first time;
- which hosts have an agent, under which names and labels;
- which profiles and integrations you applied, and where the files are;
- what you did **not** do (exporters not installed, alert receivers not
  configured, hosts you could not reach);
- how to remove it (below).

## When something fails

| What you see | Cause | What to do |
|---|---|---|
| `curl …/healthz` times out or is refused | no route to the server, or it is not running | check the server's containers and port; then DNS, firewall, VPN. Ask before changing a firewall |
| Agent log: `x509: certificate signed by unknown authority` | the server has its own authority and the agent has no `ca_pin` | add `ca_pin` to `/etc/vigil/agent.yaml` under `server:` and restart the agent |
| Agent log: `certificate is valid for …, not <address>` | the server runs in a container and cannot see the address agents use | add that address to `tls.names` in the server's config, restart the server |
| Agent log: `certificate authority has fingerprint … not the expected …` | the fingerprint is for another server, or the server's `data_dir` was replaced | read the fingerprint again from the server; do not disable checking |
| `vigil-agent enroll`: `not known` / `expired` / `used as often as it allows` | the enrolment token is wrong, old, or used up | create a new one |
| `vigil-agent enroll`: `already enrolled` | that agent id has a token | ask the person: revoke it (Fleet → Manage) or use another `-id` |
| Agent log: `HTTP 401` | wrong or revoked token | for a self-enrolled agent that was re-installed, delete `/var/lib/vigil/spool/agent-token` so it enrols again |
| Agent log: `HTTP 403 … different agent` | the token belongs to another agent id | `agent_id` must be the one the token was made for |
| Agent log: `the server now expects https` | `server.url` still says `http://` | change it; nothing was lost meanwhile |
| Agent log: `HTTP 429` | the agent sends more than the server's limits | find what grew before raising limits; [Troubleshooting](troubleshooting.md) |
| Fleet shows `config_state: error` | the agent could not apply what a profile gives it; the reason is next to it | fix the profile. A server newer than the agent can send settings the agent does not know: update the agent |
| Profile refused on apply | it would break a named agent's configuration | read the reason, fix the file |
| The agent is up but Explore shows nothing | the metric store hides the newest 30 seconds; or the time range is wrong | wait, widen the range, check the host's clock |
| `journald logs disabled: journalctl not found` | the agent runs in a container | host install, or accept that there are no journald logs |

More cases: [Troubleshooting](troubleshooting.md).

## Upgrading and removing

**Upgrade**: replace the binary (or image) and restart; nothing is lost
across a restart. When versions differ, update **agents before the server**
if the new version adds settings you intend to use, and never leave the
server older than what you are about to configure.

A host still running the old all-in-one `vigil agent` (or `vigil server`,
`vigil relay`) moves to `vigil-agent` (`vigil-server`, `vigil-relay`):
copy the new binary, point the unit's `ExecStart` at it without the
subcommand, restart. Configs and state stay. See [Install → From `vigil`
to the separate binaries](install.md#from-vigil-to-the-separate-binaries).

**Remove an agent** (ask first):

```bash
systemctl disable --now vigil-agent
rm -f /etc/systemd/system/vigil-agent.service /usr/local/bin/vigil-agent
rm -rf /etc/vigil /var/lib/vigil
```

and forget it on the server: Fleet → Manage → Forget, or
`curl -sk -X DELETE -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://SERVER:8080/api/v1/agents/NAME`.

**Remove the server** (this deletes all collected data; ask twice):

```bash
cd /opt/vigil && docker compose down -v && cd / && rm -rf /opt/vigil
```

## What not to do

- Do not put tokens in profiles, in git, in command lines that end up in
  shell history, or in your messages to the person.
- Do not set `tls_insecure_skip_verify` to get past a certificate error;
  find out why the certificate does not match.
- Do not open the server's port to the internet to make an agent connect.
  Use a private network, a VPN, or a relay.
- Do not install exporters, change firewalls, edit reverse proxies or
  remove other monitoring on your own initiative.
- Do not switch runtime events on fleet-wide in one step.
- Do not report success on "the service is active". The server's view
  (`/api/v1/agents`, a query that returns data) is what counts.
