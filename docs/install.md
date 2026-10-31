# Install

You need one **server** and an **agent** on each host you want to watch.
Each is a static binary of its own: `vigil-server` and `vigil-agent`, plus
`vigil-relay` for sites whose agents cannot reach the server.

Having an AI assistant do it? Give it [Install with an AI
assistant](install-by-ai.md): the same steps written as a runbook, with the
questions to ask first and a check after every stage.

## Download a release

Each release on [GitHub](https://github.com/ijcodelabs/vigil/releases) has
an archive per binary and architecture (the binary, the licence, the
README and its systemd unit), `SHA256SUMS`, and a signature of the
checksums. What changed and what to do when upgrading is in its notes,
which are also collected in `CHANGELOG.md` at the top of the repository.

```bash
v=0.1.0
base=https://github.com/ijcodelabs/vigil/releases/download/v$v
curl -fLO $base/vigil-agent-$v-linux-amd64.tar.gz
curl -fL -O $base/SHA256SUMS -O $base/SHA256SUMS.sigstore.json
```

Check that the files are the ones the release workflow built. With
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/):

```bash
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/ijcodelabs/vigil/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c SHA256SUMS
```

or with the GitHub CLI, per archive: `gh attestation verify
vigil-agent-$v-linux-amd64.tar.gz -R ijcodelabs/vigil`. Then unpack and
install the binary:

```bash
tar xzf vigil-agent-$v-linux-amd64.tar.gz
install -m 755 vigil-agent-$v-linux-amd64/vigil-agent /usr/local/bin/
```

Container images are `ghcr.io/ijcodelabs/vigil-agent`, `vigil-server` and
`vigil-relay`, tagged with the version (`0.1.0`) and its minor (`0.1`),
for linux/amd64 and linux/arm64, signed the same way:

```bash
cosign verify ghcr.io/ijcodelabs/vigil-agent:$v \
  --certificate-identity "https://github.com/ijcodelabs/vigil/.github/workflows/release.yml@refs/tags/v$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Build

To build from source instead, Go 1.26 or newer is the only requirement.

```bash
make build      # bin/vigil-agent, bin/vigil-server, bin/vigil-relay, bin/vigil for this machine
make linux      # the same as bin/<name>-linux-amd64 and bin/<name>-linux-arm64
```

`vigil` itself holds the operator tools (`vigil apply`, `vigil tracegen`).
For one release it can also still run as agent, server and relay; see
[Upgrading](#upgrading).

The Linux binaries are statically linked: copy one file to any Debian, Red
Hat or Ubuntu host and it runs. There are no packages to install.

## The server

The server needs VictoriaMetrics (metrics) and VictoriaLogs (logs) next to
it. They do not need to be reachable from anywhere else.

### With containers

`deploy/docker-compose.yml` starts all three. Choose two secrets first:

```bash
cat > deploy/.env <<EOF
VIGIL_INGEST_TOKEN=$(openssl rand -hex 24)
VIGIL_ADMIN_TOKEN=$(openssl rand -hex 24)
EOF
docker compose -f deploy/docker-compose.yml up -d     # or: podman-compose
```

The server then listens on port 8080.

### Without containers

Run the two stores as their own services (each is a single binary from
VictoriaMetrics' releases), copy `bin/vigil-server-linux-amd64` to
`/usr/local/bin/vigil-server`, then:

```bash
vigil-server -config /etc/vigil/server.yaml
```

```yaml
# /etc/vigil/server.yaml
listen: ":8080"
data_dir: /var/lib/vigil                 # users, profiles, silences, captures
external_url: https://vigil.example.com  # used for links in notifications
storage:
  metrics_url: http://127.0.0.1:8428     # VictoriaMetrics
  logs_url: http://127.0.0.1:9428        # VictoriaLogs
admin_token: ${VIGIL_ADMIN_TOKEN}        # ${VAR} is read from the environment
ingest_token: ${VIGIL_INGEST_TOKEN}      # optional, see "Tokens" below
```

All server settings:

| Setting | Meaning | Default |
|---|---|---|
| `listen` | address to serve on | `:8080` |
| `data_dir` | where the server keeps its own state | `/var/lib/vigil` |
| `external_url` | how people reach the server | `http://localhost:<port>` |
| `storage.metrics_url`, `storage.logs_url` | the two stores | required |
| `storage.traces_url` | VictoriaTraces, for [traces](apm.md); optional | none |
| `admin_token` | for scripts, and the very first sign-in | required |
| `ingest_token` | a token any agent may use | none |
| `heartbeat_timeout` | an agent counts as down after this silence | `60s` |
| `captures_max_bytes` | disk budget for captures; oldest go first | 2 GiB |
| `interval`, `scrape` | targets the server scrapes itself, for hosts without an agent | none |
| `costs` | prices for language-model tokens and GPU hours; see [AI costs](finops.md#prices) | none |
| `ai` | a language model that explains incidents on the Home page and log lines in Explore, with a monthly budget; its key comes from `api_key_file` or, without it, from your OpenBao or Vault ([Secrets](secrets.md)); see [Home](home.md#ai-explanations) | off |
| `limits` | what one agent may send, and how much at once; see [Limits](#limits) | see there |
| `trusted_proxies` | reverse proxies whose `X-Forwarded-For` is believed; see [Encryption](#encryption) | none |
| `tls.self_signed` | serve HTTPS with the server's own certificate authority | off |
| `tls.cert_file`, `tls.key_file` | serve HTTPS with a certificate you supply | none |
| `tls.names` | extra names or addresses for the server's own certificate | none |

### Encryption

Without a `tls` section the server speaks plain HTTP and says so in its log
at start: tokens, logs and metrics cross the network readable. Pick one of
three ways to change that.

**The server's own certificate authority.** Nothing else to install:

```yaml
tls:
  self_signed: true
external_url: https://vigil.example.com:8080
```

On first start the server creates an authority in `data_dir/pki` and serves
with a certificate signed by it, renewed by itself. The authority has a
fingerprint, printed in the log (`ca_pin=sha256:…`) and shown under
**Fleet → Enrolment**. An agent that is given the fingerprint fetches the
authority's certificate once, accepts it only if the fingerprint matches,
and from then on talks to nobody else:

```yaml
server:
  url: https://vigil.example.com:8080
  ca_pin: sha256:9d0ef263…
```

The interface fills `ca_pin` into every snippet it shows, so normally you
copy it without thinking about it. Agents may reach the server by any name
or address; the certificate covers whichever they use. One exception: in a
container with a published port the server never sees the host's address
that agents connect to, so list it under `tls.names` (unless it is the
host in `external_url`, which is always included). Your browser does
not know the authority and warns on first visit; to stop that, import
`data_dir/pki/ca.pem` (also at `/api/v1/ca`) into the browser or system
trust store. Keep `data_dir/pki/ca.key` private and in your backups: with a
new authority, every agent needs the new fingerprint.

**Your own certificate.** From your company's authority or Let's Encrypt:

```yaml
tls:
  cert_file: /etc/vigil/tls/fullchain.pem
  key_file: /etc/vigil/tls/key.pem
```

Agents then need no `ca_pin` if the host trusts the issuer; for a private
issuer set `server.ca_file` on the agents. Restart the server after
replacing the files.

**A reverse proxy** (nginx, Caddy, HAProxy, a load balancer) that does TLS
in front of the server. Set `external_url` to the `https://` address people
use, and tell the server which proxy to believe:

```yaml
external_url: https://vigil.example.com
trusted_proxies: [172.18.0.10]        # addresses or networks (10.0.0.0/8)
```

Without `trusted_proxies` every visitor seems to come from the proxy's
address, and the limit on failed sign-ins would lock everybody out
together. With it, the server takes the client's address from the
`X-Forwarded-For` the proxy sets (and from nobody else: a client's own
header is ignored), marks its cookie Secure, and sends HSTS. The proxy must
set `X-Forwarded-For` itself rather than pass on what the client sent.

If only people should reach the server through the proxy and agents use
another address, let the proxy refuse what only agents send: `POST` to
`/api/v1/write`, `/api/v1/logs`, `/api/v1/traces`, `/api/v1/events`,
`/api/v1/captures`, `/api/v1/agent/config` and `/api/v1/enroll`.

Switching an existing server to HTTPS: agents whose file still says
`http://` keep their data in the spool and log `the server now expects
https` until their `server.url` is changed.

What is not there yet: agents prove who they are with their token, not with
a client certificate.

### Limits

Each agent already limits itself (`limits` in its config). The server has
limits of its own, so that one agent with a mistake in its config, or one
that is not yours, cannot fill the stores or starve the others:

```yaml
limits:
  samples_per_second: 20000        # per agent, averaged over a minute
  log_bytes_per_second: 4194304    # per agent, averaged over a minute
  max_in_flight: 512               # batches being stored at the same time, all agents
  agents:                          # other values for the agents named
    big-db-01: {samples_per_second: 100000}
    lab-07: {log_bytes_per_second: -1}   # negative: no limit
```

The values shown are the defaults. They are well above what an agent sends
with its own defaults (100,000 series every 15 seconds is under 7,000
samples a second), so normally nobody notices them.

An agent over its limit gets `429` for the batch and sends it again later.
It falls behind, its spool fills, and it then drops its own oldest data:
the server takes no more than the limit, and nothing is dropped silently.
It shows in three places:

- **Fleet → Agents**: "over limit: metrics" on the agent.
- The agent's log: `push failed, will retry … HTTP 429`.
- Metrics: `vigil_agent_limited_batches_total{signal}` (written by the
  server) and `vigil_agent_dropped_total{reason="spool_full"}` (by the
  agent). An alert on the first is worth having:

  ```yaml
  - alert: AgentOverServerLimit
    expr: increase(vigil_agent_limited_batches_total[10m]) > 0
    for: 10m
    annotations:
      summary: 'The server is holding back {{ $labels.signal }} from {{ $labels.agent }}'
  ```

Because limits are averaged over a minute, an agent may send a minute's
worth at once; one that was cut off for hours catches up at the speed of
the limit. When more than `max_in_flight` batches arrive at the same time
the rest get `503` and come back a few seconds later.

### Sign-in protection

- Ten failed sign-ins from one address, or twenty for one account from
  anywhere, block further tries for five minutes (`429`). Failed attempts
  are in the server's log with the address.
- A session lasts seven days. Changing a password signs the user out
  everywhere else; an admin changing someone's password or role signs that
  user out everywhere.
- Viewers can read profiles and agent configs, with the credentials in them
  (`bearer_token`, `password` and the like) shown as `(hidden)`.

### First sign-in

Open the server's address in a browser and sign in as `admin` with the
**admin token** as the password. Go to **Users** and create an account for
yourself. From then on the admin token no longer signs in to the interface;
it keeps working for scripts.

## An agent on a Linux host

1. Copy the binary:

   ```bash
   scp bin/vigil-agent-linux-amd64 host:/usr/local/bin/vigil-agent
   ```

2. In the interface, **Fleet → Enrolment → New enrolment token**. Run the
   command it shows on the host, as root:

   ```bash
   vigil-agent enroll -server https://vigil.example.com:8080 \
     -token vge_… -ca-pin sha256:… \
     -label env=prod -label role=web
   ```

   It exchanges the enrolment token for a token that only this host can
   use, and writes the agent's file:

   ```yaml
   # /etc/vigil/agent.yaml
   server:
     url: https://vigil.example.com:8080
     token: vgl_…
     ca_pin: sha256:…
   agent_id: web-01
   labels:
     env: prod
     role: web
   ```

   That is the whole file. What the agent collects comes from the server's
   profiles (see [Fleet](fleet.md)). You can also put collection settings
   in this file; they apply as long as no profile matches the agent (see
   [Agent configuration](configs.md)).

   `-id` sets another name than the host name, `-config` another path.
   `-ca-pin` is only for a server with its own certificate authority. For
   a single host you can also use **Fleet → Add agent**, which shows the
   same file to copy by hand.

3. Install the service:

   ```bash
   cp deploy/vigil-agent.service /etc/systemd/system/
   touch /etc/vigil/agent.env && chmod 600 /etc/vigil/agent.env   # for ${VAR} values, may stay empty
   systemctl enable --now vigil-agent
   ```

4. Within a minute the agent shows on the **Fleet** page with a green dot,
   and `host_*` metrics appear in **Explore**.

The agent runs as root in that unit. Root is needed for reading all log
files, for other users' processes, and for runtime events; plain metrics
work without it.

## An agent in a container

```bash
docker run -d --name vigil-agent --pid host --network host \
  -e HOST_PROC=/host/proc -e HOST_SYS=/host/sys -e HOST_ETC=/host/etc -e HOST_ROOT=/host/root \
  -v /proc:/host/proc:ro -v /sys:/host/sys:ro -v /etc:/host/etc:ro -v /:/host/root:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/vigil:/etc/vigil:ro -v /var/lib/vigil:/var/lib/vigil \
  ghcr.io/ijcodelabs/vigil-agent:0.1.0 -config /etc/vigil/agent.yaml
```

That is the released image (see [Download a release](#download-a-release)
to verify it). Your own is built with `docker build --target agent -t
vigil-agent:dev .` (`--target server` and `--target relay` for the
others).

The `HOST_*` variables tell the agent where the host's files are, so the
numbers describe the host and not the container. Add `--privileged` if you
use runtime events. In a container there is no `journalctl` or `systemctl`,
so journald logs and systemd unit states need a host install.

## Kubernetes

See [Kubernetes](kubernetes.md): a DaemonSet for the nodes and one small
Deployment for the cluster as a whole.

## Many hosts at once

One enrolment token serves any number of hosts, so it fits in an image,
an Ansible role or cloud-init. Either run `vigil-agent enroll` as above in your
provisioning, or ship the same file everywhere and let each agent enrol
itself when it first starts:

```yaml
# /etc/vigil/agent.yaml, identical on every host
server:
  url: https://vigil.example.com:8080
  enroll_token: ${VIGIL_ENROLL_TOKEN}
  ca_pin: sha256:…
labels:
  env: prod
```

The agent enrols under its host name (or `agent_id`), keeps its own token
in `spool_dir/agent-token`, and never uses the enrolment token again; the
token may expire or be revoked afterwards without affecting hosts that are
already in. While the server cannot be reached the agent keeps trying and
collects into its spool.

When you create the token you choose how long it is valid and, if you
like, how many hosts may use it. Rules worth knowing:

- A name can be enrolled once. A second host asking for the same name is
  refused, so the holder of an enrolment token cannot take the place of an
  existing host. To re-install a host, **Manage → Revoke token** (or
  **Forget**) on the Fleet page first, and delete `spool_dir/agent-token`
  on the host if it still has one.
- An enrolment token cannot read data or send any; it only makes agent
  tokens.
- Refused attempts are logged by the server with the address they came
  from, and slowed down like failed sign-ins.

## Hosts that cannot reach the server

Put a [relay](relay.md) where they can reach it: one host per site or
cluster talks to the server, and the agents talk to that host.

## Tokens

- **Enrolment tokens** (`vge_…`, Fleet → Enrolment) are exchanged by hosts
  for a token of their own. Short-lived; for adding hosts.
- **Per-agent tokens** (`vgl_…`) work only for the agent id they were made
  for. Enrolment creates them; Fleet → Add agent makes one by hand.
- **`ingest_token`** in the server's file is accepted from any agent. It is
  convenient for a first test; leave it out to accept per-agent tokens
  only.
- **`admin_token`** is for the API and `vigil apply`.
- **Read-only tokens** (`vgr_…`, Users → Read-only tokens) let a tool read
  what a viewer can see and change nothing: for [Grafana](grafana.md) and
  for scripts.

The server stores only hashes of enrolment, agent and read-only tokens;
each is shown once.

## Check that it works

```bash
curl -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" https://vigil.example.com/api/v1/agents
curl -H "Authorization: Bearer $VIGIL_ADMIN_TOKEN" \
     --data-urlencode 'query=vigil_agent_up' https://vigil.example.com/api/v1/query
```

Data becomes visible to queries about 30 seconds after it was sent.

## Upgrading

Replace the binary and restart. The server's state is in `data_dir`; an
agent's unsent data and its place in each log file are in its `spool_dir`,
so nothing is lost or sent twice across a restart.

Read the release's upgrade notes first. A server accepts agents of its own
minor version and of the one before (a 0.3 server takes 0.2 and 0.3
agents), so upgrade the server first and the agents after, one minor
version at a time.

### From `vigil` to the separate binaries

Until now one `vigil` binary was agent, server and relay. They are now
`vigil-agent`, `vigil-server` and `vigil-relay`; the agent is about 21 MB
instead of 51 and uses less memory, because it no longer carries the
server. Config files, tokens, `data_dir` and `spool_dir` stay as they are.

1. Copy the new binary next to the old one, e.g.
   `/usr/local/bin/vigil-agent`.
2. Point the unit at it: `ExecStart=/usr/local/bin/vigil-agent -config
   /etc/vigil/agent.yaml` (the files in `deploy/` already do), then
   `systemctl daemon-reload && systemctl restart vigil-agent`. The same for
   the server (`vigil-server -config …`) and a relay (`vigil-relay -config
   …`). In Kubernetes, use the `vigil-agent` and `vigil-relay` images and
   drop the first argument (`agent`, `relay`) from `args`.
3. Claude Code hooks: change `vigil claude-hook` to `vigil-agent
   claude-hook` in the settings file.

The old `vigil agent`, `vigil server`, `vigil relay`, `vigil enroll` and
`vigil events` keep working for one release and log a warning; `vigil
claude-hook` keeps working silently. Upgrade agents and server in either
order: what they send each other has not changed.
