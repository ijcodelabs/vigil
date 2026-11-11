# Portun and Portun Jump

Portun handles access requests, approvals and
grants; Portun Jump gives people recorded SSH and Kubernetes access through a
gateway and agents. Each works without vigil. vigil watches them through
Portun's published **observability contract** (v1, `docs/observability.md`
in Portun Jump's repository): Prometheus metrics, audit events as
OpenTelemetry logs, and nothing else. There is no shared code.

Portun was called Boop until November 2026, and vigil v0.2.0's integrations
were `boop`, `boop_jump_gateway` and `boop_jump_agent`. Portun sends only the
new names, so a profile that still names an old integration is refused with
the new name to use.

Four integrations come with vigil:

| Integration | Where | What |
|---|---|---|
| `portun` | the host that reaches Portun | Portun's own `/metrics`, the tenant export (the servers Portun Jump knows, also the offline ones), Portun's audit events, and the SSH check below |
| `portun_jump_gateway` | the gateway's host | the gateway's `/metrics`: sessions, refusals, the recording spool, per-agent detail |
| `portun_jump_agent` | each Jump agent's host | the agent's `/metrics`: its tunnel, certificate, accepted and refused streams |
| `sshd` | the hosts you guard | sshd's login lines from the journal, taken apart; useful on its own as an SSH logins dashboard |

The alert rules carry the same names as Portun's own rule files
(`PortunDown`, `PortunJumpSpoolFull`, …), so a rule file that also loads Portun's
copy replaces them by name instead of alerting twice. Where Portun's rule
uses `increase()`, vigil's compares with the value a while ago, because
the store counts a counter it sees for the first time as risen from zero.

## What vigil trusts

- **Portun is the record of truth.** What vigil stores is a copy for
  display and correlation. Nothing in vigil grants access, approves a
  request or clears an alert because of a Portun event, and a gap in vigil's
  copy says nothing about Portun's record.
- **Audit events count only from pinned sources:** they must arrive at a
  vigil agent named in `event_agents`, with a token whose name is in
  `emitters`, from a sender whose `service.instance.id` is that same name.
  Anything else that calls itself Portun's audit (`logger="portun.audit"`) is
  shown, but raises `PortunEventsFromUnknownSource`.
- **Names of people never reach vigil.** Portun sends pseudonyms
  (`actor_ref`), which only Portun can resolve, behind its own sign-in.
- Portun Jump's agent id arrives in a label named `agent`. vigil's own `agent`
  label says which vigil agent collected the data, so Portun's is kept as
  **`exported_agent`** (see [Metrics](metrics.md)).
- Every viewer of vigil can see these metrics and events in Explore. A
  dashboard does not hide data from people who can query it.

## Set up

### Portun's metrics and the tenant export

On the host whose vigil agent will scrape Portun, put the two tokens in files
the agent can read:

```bash
echo -n "$PORTUN_METRICS_TOKEN"   | sudo tee /etc/vigil/portun-metrics-token >/dev/null
echo -n "bxp_…"                | sudo tee /etc/vigil/portun-export-token  >/dev/null   # portun admin jump export-token
sudo chmod 600 /etc/vigil/portun-*-token
```

The export token is bound to one tenant: it decides whose servers vigil
sees. Then, in a profile:

```yaml
select: ['role="portun"']
integrations:
  portun:
    metrics_target: "localhost:3000"
    export_target: "portun.example.com"
    export_scheme: https
```

Without a tenant export, switch its job off in the same profile:
`config: {scrape: {static: [{job: portun-export, enabled: false}]}}`.

### The gateway and the agents

```yaml
select: ['detected_portun_jump_gateway="true"']
integrations:
  portun_jump_gateway: {metrics_target: "127.0.0.1:9464"}   # its --metrics-listen
```

and the same with `portun_jump_agent` (default `127.0.0.1:9465`). Their
`/metrics` have no authentication, so they belong on loopback or a private
address. Detection finds them by process name (`jump-gateway`,
`jump-agent`).

### Audit events

Portun, the gateway, the agent and Portun Chat send their events to a vigil
agent's OpenTelemetry endpoint, each with its own token. On that vigil
agent:

```bash
# one line per emitter: the name is its service.instance.id (Portun: PORTUN_OTLP_INSTANCE, default "portun"; a gateway: its --id)
sudo tee /etc/vigil/otlp-tokens >/dev/null <<'EOF'
portun       <a long random token>
gw-euw1    <another one>
portun-chat  <a third one>
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
  portun:
    event_agents: "portun-host"            # vigil agent ids, a regular expression
    emitters: "portun|gw-euw1|portun-chat"   # names from the tokens file
    tenant_ref: <the tenant_ref, 32 hex characters>
```

Point Portun at it with `PORTUN_OTLP_LOGS_URL=http://127.0.0.1:4318/v1/logs` and
`PORTUN_OTLP_TOKEN_FILE`, the gateway, the agent and Portun Chat with
`--otlp-logs-url` and `--otlp-token-file`. They require `https` unless the
endpoint is on loopback.

Portun's emitters can use a client certificate instead of a token, kept and
renewed by the vigil agent on their host: `PORTUN_OTLP_CA_FILE`,
`PORTUN_OTLP_CERT_FILE` and `PORTUN_OTLP_KEY_FILE` on Portun, `--otlp-ca-file`,
`--otlp-cert-file` and `--otlp-key-file` on the gateway, the agent and Portun
Chat. See APM, "Client certificates from the server".

vigil keeps no person ids. Set `PORTUN_OTLP_PEOPLE=off` on Portun and
`--otlp-people=off` on the gateway and the agent, so that their events
leave out the person pseudonyms (`actor_ref`, `approver_ref`); Portun Chat's
events never carry them. The `portun` integration drops those two fields on
arrival anyway (`otlp.logs.drop_attributes`), and the SSH logins
integration masks the pseudonym inside a Portun Jump certificate's Key ID
(`portun-jump:v1:masked:…`).

### Portun Chat's work on the charts

Portun Chat's events say what happened to its items and workroom runs: an
incident opened or moved, an investigation started, proposed, accepted, a
run started or ended. vigil draws those of a service's items as **markers**
on the service's charts and on the SLO page of an SLO with that service
(the item's service field: the alert's `service` or `job` label). Hover a
marker for what it was; click it to open "Did a change help?" at its time.
An incident whose evidence an investigation read shows its Portun item and
that item's events ("In Portun: SHOP-12"). Portun is the record; these are its
events as received. API: `GET /api/v1/markers`, see [API](api.md). More on tokens and TLS in [Services and traces](apm.md#tokens-and-tls).

### SSH logins that did not come through Portun Jump

Portun Jump logs in to a server's own sshd with a short certificate signed by
the tenant's SSH CA, whose Key ID names the session and grant. So every
Jump login leaves an sshd line that says so, and any other login does not.

1. Switch on `sshd` for the hosts to guard (it needs journald).
2. Tell `portun` which hosts must only be reached through Jump, and the
   tenant CA's fingerprint (Jump's admin settings, or
   `ssh-keygen -lf ca.pub`):

   ```yaml
   integrations:
     sshd: {}
     portun:
       guarded_agents: "web-prod-.*|db-prod-01"
       tenant_ca: "SHA256:<the tenant CA's fingerprint>"
   ```

`SshLoginNotThroughPortunJump` fires for an accepted login on a guarded host
unless all three hold: the key is a certificate, its Key ID has the form
`portun-jump:v1:<actor_ref>:<session>:<grant>`, and the line's **last** field
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
the certificate's serial; Portun's own records do.

## Alerts

| Alert | Means |
|---|---|
| `PortunDown`, `PortunExportDown` | a scrape of Portun or its export fails (address, token, edge) |
| `PortunShardUnreachable`, `PortunJobsStalledFulfilment`, `PortunAccessOverdue`, `PortunRevokeStuck`, `PortunOutboxBacklog`, `PortunApprovalsWaiting`, `PortunJumpRecordingProblems`, `PortunHttpErrors` | Portun's own rules |
| `PortunSecretsStoreSealed` | Portun cannot use OpenBao |
| `PortunExportTruncated` | the export hit its limit and left servers out |
| `PortunEventsDropped`, `PortunJumpEventsDropped` | events were dropped on the way to vigil: vigil's copy has a gap |
| `PortunEventsFromUnknownSource` | an event claiming to be Portun's came from a source that is not pinned |
| `PortunEventsForAnotherTenant` | a pinned source sent events for another tenant (or `tenant_ref` is not set) |
| `PortunBreakGlassUsed` | someone used break-glass access |
| `SshLoginNotThroughPortunJump` | above |
| `PortunJumpGatewayDown`, `PortunJumpSpool*`, `PortunJumpRecording*`, `PortunJumpAgentCertExpiring`, `PortunJumpAgentRenewalsRefused`, `PortunJumpDenialsSpike`, `PortunJumpAgentTunnel*` | Portun Jump's own rules |

## Dashboards

**Portun**: requests waiting, grants, a **map** (admins only), servers Portun
Jump knows (connected or not, last seen), resources, audit events as
received, SSH logins without a Jump Key ID.

The map shows Portun, its gateways, the agents grouped by host group, and
the servers and clusters they reach. A node is red when it is down or
offline, yellow when, say, its certificate has less than a week left; open
sessions are links that move. The card of an agent shows its host name,
tags, version, certificate time left and when it was last seen; a
gateway's its recording spool; Portun's its waiting requests, outbox lag and
whether its secrets store is sealed. See [Dashboards](dashboards.md#maps). **Portun Jump gateway**, **Portun Jump agents** and **SSH logins**
come with their integrations.

Not built yet: session markers on charts, a request-access link on
incidents, roles that decide who may see what (until then the map is for
admins).

## Investigating an incident from Portun Chat

Portun Chat makes an incident item from vigil's alert webhook. To let an
agent's run in a workroom look at what vigil knows about that incident,
give Portun Chat an **issuer token** (Users → Read-only tokens → New token →
It may: only issue evidence grants). That token reads nothing itself. When
someone presses **🔎 Investigate** on an incident, Portun Chat asks for an
[evidence grant](api.md#evidence-grants) with the alert's labels, for one
read and five minutes, reads the incident's masked evidence once and gives
it to a read-only workroom run as a file; the run never gets a token for
vigil. Its answer goes back to the incident as a proposal. Every grant
shows on the Users page (purpose "<item key> investigation"), where an
admin can end it.
