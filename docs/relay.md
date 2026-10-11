# Relays

A relay is a small vigil process that agents send to instead of the
server. It passes everything on. Use one when

- hosts in a network segment, a site or a Kubernetes cluster **cannot reach
  the server**, but one of them (or a gateway) can;
- you want **one connection out** per site or cluster instead of one per
  agent, for firewall rules or to keep the number of connections to the
  server down.

```
agents ──► relay ──► server
           (one per site or cluster)
```

A relay keeps nothing and decides nothing:

- **Tokens are still checked by the server.** Enrolment works through a
  relay, and an agent's token is refused through a relay exactly as it
  would be directly. A relay does not need a token of its own.
- **Only what agents send is passed on**: metrics, logs, spans, detections,
  captures, the config poll and enrolment. The interface and the rest of
  the API answer 404 on a relay, so a relay does not open the server to the
  network it sits in.
- **Nothing is buffered.** If the relay cannot reach the server, agents are
  told to try again and keep the data in their own spools, as they do when
  they talk to the server directly. A relay can be restarted or replaced at
  any time.
- Config changes, profiles and rollouts reach agents through it unchanged.

## Run one

```yaml
# /etc/vigil/relay.yaml
listen: ":8080"
relay_id: site-zagreb            # shown next to its agents; default: the host name
server:
  url: https://vigil.example.com:8080
  ca_pin: sha256:…               # if the server uses its own certificate authority
tls:
  self_signed: true              # encrypt what agents send to the relay
```

```bash
vigil relay -config /etc/vigil/relay.yaml
```

`deploy/vigil-relay.service` is a systemd unit for it; it needs no root.

Then point the agents at the relay instead of the server. Everything else
in their file stays as it is:

```yaml
# /etc/vigil/agent.yaml
server:
  url: https://relay.site.internal:8080
  enroll_token: vge_…            # or token: for an agent that already has one
  ca_pin: sha256:…               # the RELAY's fingerprint, from its log
```

`vigil enroll -server https://relay.site.internal:8080 …` works the same
way.

| Setting | Meaning | Default |
|---|---|---|
| `listen` | address agents connect to | `:8080` |
| `relay_id` | name shown on the Fleet page | host name |
| `server.url` | the server, or another relay | required |
| `server.ca_pin`, `server.ca_file` | how the relay checks the server | system authorities |
| `tls.self_signed` | the relay makes its own certificate authority in `data_dir/pki` | off |
| `tls.cert_file`, `tls.key_file` | or a certificate you supply | none |
| `data_dir` | where that authority is kept | `/var/lib/vigil-relay` |
| `max_in_flight` | requests passed on at once; more are told to retry | 256 |

## Encryption

There are two connections, each with its own certificate:

- **Agent to relay.** With `tls.self_signed` the relay has its own
  authority; its fingerprint is in the relay's log at start
  (`ca_pin=sha256:…`) and that is what goes into the agents' `ca_pin`. It
  is not the server's fingerprint. Keep the relay's `data_dir`: a new
  authority means a new fingerprint for every agent behind it. Without a
  `tls` section the relay speaks plain HTTP and warns about it; tokens then
  cross that network readable.
- **Relay to server.** Checked like an agent checks the server: `ca_pin`
  for a server with its own authority, `ca_file`, or the system's
  authorities.

## Kubernetes

`deploy/k8s-relay.yaml` runs a relay as a Deployment with a Service. Point
the agents' `server.url` in both agent manifests at
`http://vigil-relay.vigil.svc:8080`. Inside the cluster that manifest uses
plain HTTP (the connection out to the server is encrypted); if the cluster
network is not trusted, give the relay a certificate.

## What you see

- **Fleet → Agents** shows "via site-zagreb" under each agent that came
  through a relay; with several in a row, the one nearest the agent first.
  The filter accepts `via=site-zagreb`.
- The agent's address shown by the server is the relay's.
- A relay answers `GET /healthz`, and `GET /metrics` with a handful of
  counters. Let the agent on the relay's host scrape it:

  ```yaml
  scrape:
    static:
      - {job: vigil-relay, targets: ["localhost:8080"]}
  ```

  `vigil_relay_last_success_age_seconds` growing, or
  `vigil_relay_failures_total` rising, means the relay has lost the server.

## Limits

- A relay is one more thing that can be down. Agents behind it buffer, up to
  their `spool_max_bytes`; run two relays behind one name or address if that
  is not enough.
- Failed enrolment attempts are slowed down per source address, and behind
  a relay every agent has the relay's address: one host retrying a bad
  enrolment token can hold up enrolment for its whole site for a few
  minutes.
- The "via" shown is what the relay says; it is for orientation, not proof.
