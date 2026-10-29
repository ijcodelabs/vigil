# Boop and Boop Jump

Boop handles access requests, approvals and
grants; Boop Jump gives people recorded SSH and Kubernetes access through a
gateway and agents. Each works without vigil. vigil watches them through
Boop's published **observability contract** (v1, `docs/observability.md`
in Boop Jump's repository): Prometheus metrics, audit events as
OpenTelemetry logs, and nothing else. There is no shared code.

Four integrations come with vigil:

| Integration | Where | What |
|---|---|---|
| `boop` | the host that reaches Boop | Boop's own `/metrics`, the tenant export (the servers Boop Jump knows, also the offline ones), Boop's audit events, and the SSH check below |
| `boop_jump_gateway` | the gateway's host | the gateway's `/metrics`: sessions, refusals, the recording spool, per-agent detail |
| `boop_jump_agent` | each Jump agent's host | the agent's `/metrics`: its tunnel, certificate, accepted and refused streams |
| `sshd` | the hosts you guard | sshd's login lines from the journal, taken apart; useful on its own as an SSH logins dashboard |

The alert rules carry the same names as Boop's own rule files
(`BoopDown`, `BoopJumpSpoolFull`, …), so a rule file that also loads Boop's
copy replaces them by name instead of alerting twice. Where Boop's rule
uses `increase()`, vigil's compares with the value a while ago, because
the store counts a counter it sees for the first time as risen from zero.

## What vigil trusts

- **Boop is the record of truth.** What vigil stores is a copy for
  display and correlation. Nothing in vigil grants access, approves a
  request or clears an alert because of a Boop event, and a gap in vigil's
  copy says nothing about Boop's record.
- **Audit events count only from pinned sources:** they must arrive at a
  vigil agent named in `event_agents`, with a token whose name is in
  `emitters`, from a sender whose `service.instance.id` is that same name.
  Anything else that calls itself Boop's audit (`logger="boop.audit"`) is
  shown, but raises `BoopEventsFromUnknownSource`.
- **Names of people never reach vigil.** Boop sends pseudonyms
  (`actor_ref`), which only Boop can resolve, behind its own sign-in.
- Boop Jump's agent id arrives in a label named `agent`. vigil's own `agent`
  label says which vigil agent collected the data, so Boop's is kept as
  **`exported_agent`** (see [Metrics](metrics.md)).
- Every viewer of vigil can see these metrics and events in Explore. A
  dashboard does not hide data from people who can query it.

## Set up

### Boop's metrics and the tenant export

On the host whose vigil agent will scrape Boop, put the two tokens in files
the agent can read:

```bash
echo -n "$APS_METRICS_TOKEN"   | sudo tee /etc/vigil/boop-metrics-token >/dev/null
echo -n "bxp_…"                | sudo tee /etc/vigil/boop-export-token  >/dev/null   # boop admin jump export-token
sudo chmod 600 /etc/vigil/boop-*-token
```

The export token is bound to one tenant: it decides whose servers vigil
sees. Then, in a profile:

```yaml
select: ['role="boop"']
integrations:
  boop:
    metrics_target: "localhost:3000"
    export_target: "boop.example.com"
    export_scheme: https
```

Without a tenant export, switch its job off in the same profile:
`config: {scrape: {static: [{job: boop-export, enabled: false}]}}`.

### The gateway and the agents

```yaml
select: ['detected_boop_jump_gateway="true"']
integrations:
  boop_jump_gateway: {metrics_target: "127.0.0.1:9464"}   # its --metrics-listen
```

and the same with `boop_jump_agent` (default `127.0.0.1:9465`). Their
`/metrics` have no authentication, so they belong on loopback or a private
address. Detection finds them by process name (`jump-gateway`,
`jump-agent`).

### Audit events

Boop, the gateway and the agent send their events to a vigil agent's
OpenTelemetry endpoint, each with its own token. On that vigil agent:

```bash
# one line per emitter: the name is its service.instance.id (Boop: APS_OTLP_INSTANCE, default "boop"; a gateway: its --id)
sudo tee /etc/vigil/otlp-tokens >/dev/null <<'EOF'
boop     <a long random token>
gw-euw1  <another one>
EOF
sudo chmod 600 /etc/vigil/otlp-tokens
```

```yaml
config:
  otlp:
    enabled: true
    tokens_file: /etc/vigil/otlp-tokens
    # listen: 0.0.0.0:4318 and tls: {cert_file, key_file} when the senders
    # are not on this host
integrations:
  boop:
    event_agents: "boop-host"            # vigil agent ids, a regular expression
    emitters: "boop|gw-euw1"             # names from the tokens file
    tenant_ref: <the tenant_ref, 32 hex characters>
```

Point Boop at it with `APS_OTLP_LOGS_URL=http://127.0.0.1:4318/v1/logs` and
`APS_OTLP_TOKEN_FILE`, the gateway and agent with `--otlp-logs-url` and
`--otlp-token-file`. Boop requires `https` unless the endpoint is on
loopback. More on tokens and TLS in [Services and traces](apm.md#tokens-and-tls).

### SSH logins that did not come through Boop Jump

Boop Jump logs in to a server's own sshd with a short certificate signed by
the tenant's SSH CA, whose Key ID names the session and grant. So every
Jump login leaves an sshd line that says so, and any other login does not.

1. Switch on `sshd` for the hosts to guard (it needs journald).
2. Tell `boop` which hosts must only be reached through Jump, and the
   tenant CA's fingerprint (Jump's admin settings, or
   `ssh-keygen -lf ca.pub`):

   ```yaml
   integrations:
     sshd: {}
     boop:
       guarded_agents: "web-prod-.*|db-prod-01"
       tenant_ca: "SHA256:<the tenant CA's fingerprint>"
   ```

`SshLoginNotThroughBoopJump` fires for an accepted login on a guarded host
unless all three hold: the key is a certificate, its Key ID has the form
`boop-jump:v1:<actor_ref>:<session>:<grant>`, and the line's **last** field
is the tenant CA. The line is read from sshd's own unit, never by the
`sshd` tag that anyone can put on a line with `logger`; an accepted line
vigil could not take apart counts as not Jump.

What this promises: best-effort notice of SSH logins through sshd that did
not come through Jump, while vigil's agent runs on the host and its root
account is not in someone else's hands.

What it does not see: logins that never touch sshd (a console, a cloud
serial console, `docker exec`, `kubectl exec`), anything a root user does
to sshd's configuration or the journal, a second trusted CA in sshd's
configuration (watch `TrustedUserCAKeys` and `sshd_config` separately),
logins while the agent was down, and Kubernetes access. It does not check
the certificate's serial; Boop's own records do.

## Alerts

| Alert | Means |
|---|---|
| `BoopDown`, `BoopExportDown` | a scrape of Boop or its export fails (address, token, edge) |
| `BoopShardUnreachable`, `BoopJobsStalledFulfilment`, `BoopAccessOverdue`, `BoopRevokeStuck`, `BoopOutboxBacklog`, `BoopApprovalsWaiting`, `BoopJumpRecordingProblems`, `BoopHttpErrors` | Boop's own rules |
| `BoopSecretsStoreSealed` | Boop cannot use OpenBao |
| `BoopExportTruncated` | the export hit its limit and left servers out |
| `BoopEventsDropped`, `BoopJumpEventsDropped` | events were dropped on the way to vigil: vigil's copy has a gap |
| `BoopEventsFromUnknownSource` | an event claiming to be Boop's came from a source that is not pinned |
| `BoopEventsForAnotherTenant` | a pinned source sent events for another tenant (or `tenant_ref` is not set) |
| `BoopBreakGlassUsed` | someone used break-glass access |
| `SshLoginNotThroughBoopJump` | above |
| `BoopJumpGatewayDown`, `BoopJumpSpool*`, `BoopJumpRecording*`, `BoopJumpAgentCertExpiring`, `BoopJumpAgentRenewalsRefused`, `BoopJumpDenialsSpike`, `BoopJumpAgentTunnel*` | Boop Jump's own rules |

## Dashboards

**Boop**: requests waiting, grants, a **map** (admins only), servers Boop
Jump knows (connected or not, last seen), resources, audit events as
received, SSH logins without a Jump Key ID.

The map shows Boop, its gateways, the agents grouped by host group, and
the servers and clusters they reach. A node is red when it is down or
offline, yellow when, say, its certificate has less than a week left; open
sessions are links that move. The card of an agent shows its host name,
tags, version, certificate time left and when it was last seen; a
gateway's its recording spool; Boop's its waiting requests, outbox lag and
whether its secrets store is sealed. See [Dashboards](dashboards.md#maps). **Boop Jump gateway**, **Boop Jump agents** and **SSH logins**
come with their integrations.

Not built yet: session markers on charts, a request-access link on
incidents, roles that decide who may see what (until then the map is for
admins).
