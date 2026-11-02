# Identity: client certificates, rotation and revocation (design, 2026-11-02)

First build done 2026-11-02 (`state.md`, "Client certificates for OTLP
senders"); steps 1–3 below and Boop Chat of step 4.

Foundations step 4's identity part (direction.md), built first for local
clients that send to an agent's OTLP endpoint (Boop's Core, Jump gateway
and Chat on the trial host), then for agents themselves. Agreed in
discussion with the owner 2026-11-02; reviewed with a second model before
building.

## What exists
- The server's own CA (`internal/pki`, `tls.self_signed`), which today
  signs only the server's leaf. Agents trust it by pin (`server.ca_pin`).
- Agents authenticate by token (per-agent token from an enrolment token).
- The agent's OTLP endpoint: server TLS from files, bearer tokens per
  sender (`otlp.tokens_file`), the token's name becomes `emitter`.

## Decisions
1. **Short-lived certificates, renewed automatically.** Client certificates
   live 24 hours and are renewed at two thirds of their life. A new key every
   time: the client (or the renewer for it) makes the key and sends a CSR;
   the server signs and never sees a private key.
2. **The host's agent renews for local clients.** A profile names the
   clients an agent may hold certificates for:
   `identity: {clients: [{name: boop-chat, cert_file, key_file, ca_file, owner, group, mode}]}`.
   The agent asks the server for each (authenticated as itself), writes the
   files atomically, and the clients re-read them when they change. Boop's
   emitters only learn to read a CA file, a certificate and a key, and to
   reload them; no renewal code in three code bases.
3. **Names are approved once on the server.** A client name is an admin's
   decision (Fleet → Clients: name, which agents may hold it). An agent asking
   for a name it is not allowed gets nothing. A compromised host can mint only
   the names listed for it.
4. **Revocation in three layers.**
   - The server stops renewing a revoked name: dead within 24 h.
   - A deny list (revoked serials and names) rides with the configuration
     agents already pull; OTLP endpoints check it in the TLS handshake and
     close open connections. Effective within the pull interval.
   - An epoch per name, in the certificate. Revoking bumps it; endpoints
     accept only the current epoch, so every certificate ever issued for
     that name stops, stolen copies too.
5. **CA rotation through a trust bundle.** Endpoints and clients trust a
   bundle (old and new CA) while a new CA takes over, then the old one goes.
   Built in from the start.
6. **The OTLP endpoint's own certificate** can come from the server too
   (`otlp.tls.from_server: {names: [192.0.2.54]}`), renewed the same way.
7. **mTLS on the OTLP endpoint**: `otlp.tls.client_certs: required | optional
   | off`. A valid certificate's name is the emitter name, as a token's is
   today; tokens stay for senders that cannot use certificates.
8. **Signs of a stolen certificate**: one name seen from two places at once
   (two source addresses, or for Boop two `seq_epoch`s in parallel) raises an
   alert.
9. **No CRLs, no OCSP.** Short lifetimes plus the deny list and epochs.
10. **Agents later**, the same way: first certificate from the enrolment
    token, renewal over the agent's own connection, deny list and epochs;
    tokens stay until every agent has a certificate.

## Certificate profile
- ECDSA P-256, signed by the server's CA, `ExtKeyUsageClientAuth` only
  (serving certificates `ServerAuth` only).
- Identity in a URI SAN: `vigil://client/<name>?epoch=<n>` (agents later:
  `vigil://agent/<id>?epoch=<n>`); no person, no host name in it.
- Validity 24 h for clients, 7 days for serving certificates; NotBefore one
  minute back for clock skew.

## Server
- Tables `identities` (kind, name, epoch, allowed agents as a selector,
  created, revoked) and `certificates` (serial, name, epoch, not_after,
  issued_to agent, issued).
- `POST /api/v1/identity/sign` (agent token): a CSR for a name it may hold →
  certificate + CA bundle. Refused for a revoked or unknown name, a CSR whose
  key is not P-256 or that asks for more than the name.
- Admin API and Fleet → Clients: list, add, revoke (deny + epoch bump),
  rotate now; every action in the server log.
- The deny list and current epochs go out with every agent's config.

## Boop
- Core `BOOP_OTLP_CA_FILE`, `BOOP_OTLP_CERT_FILE`, `BOOP_OTLP_KEY_FILE`;
  Jump gateway and agent, Chat `--otlp-ca-file`, `--otlp-cert-file`,
  `--otlp-key-file`; files re-read when they change (checked before each
  batch); a token stays possible.

## Order
1. Server: identities, signing, deny list and epochs in the config, API.
2. Agent: renewer for local clients and for its OTLP endpoint's
   certificate; mTLS on the OTLP endpoint with deny list and epoch checks.
3. Boop emitters: CA and client certificate files, reloaded.
4. UI (Fleet → Clients), docs, the trial: the OTLP gateway gets its
   certificate and checks clients; Boop Chat sends its events.
5. Later: agents' own certificates (Foundations step 4 proper).

## After review (Codex, 2026-11-02)

Where this differs from the above, this wins.

- **No paths or principals from the server.** Client slots (name, the files,
  owner, group, mode) are allowed only in the agent's **local** file, never
  in a profile: the host's owner decides where credentials go and for whom;
  the server only approves which names an agent may get. Files are written
  atomically into existing directories, never through a symlink.
- **7-day client certificates, renewed at half-life** (and serving
  certificates the same): a server outage of up to about three days stops
  nothing. 24 h was too tight for "offline for a while".
- **One revocation mechanism: epochs.** Each approved name has an epoch;
  certificates carry it; revoking bumps it (and refuses renewal until the name
  is approved again, at the new epoch). Endpoints get name → current epoch
  with their config and accept only that epoch. No serial deny lists.
  Endpoints keep the last state they got; while the server is unreachable a
  stolen current certificate works until it expires (at most 7 days) —
  availability wins, and that is written down.
- **Strict signing:** the CSR's signature is checked, the key must be P-256,
  everything the CSR asks for (subject, SANs, extensions) is ignored; the
  server builds the template: exactly one URI SAN
  `vigil://client/<name>?epoch=<n>`, `ClientAuth` only, no CA. The endpoint
  parses that URI exactly and ignores the Common Name.
- **Existing connections:** the endpoint checks each request's certificate
  against the current epochs (not only at the handshake), so a revoked
  client's keep-alive connection stops at its next request.
- **Token and certificate:** when a request has both, they must name the
  same emitter; otherwise refused.
- **The CA key** stays online in `data_dir/pki/ca.key` for now (one CA, as
  today). An offline root with an online intermediate, and the trust bundle
  for rotation, are a later step; until then rotating the CA means
  re-pinning every agent.
- **Clock skew:** NotBefore five minutes back; hosts need NTP (said in the
  docs).
- Out of the first build: Fleet → Clients UI (an admin API and `vigil`
  commands instead), anomaly alerts, the trust bundle, agents' own
  certificates.

### First build
1. `internal/pki`: sign a client (and serving) certificate from a CSR,
   strictly. Server: table `identities` (name, allowed agents as a regexp,
   epoch, revoked), `POST /api/v1/identity/sign` (agent token; name allowed
   for that agent, not revoked), admin `GET/PUT/DELETE /api/v1/identities/…`
   and `POST …/revoke`; epochs in each agent's config.
2. Agent: local `identity.clients` slots and `otlp.tls.from_server`; a
   renewer (key, CSR, sign, atomic write) at half-life and at start.
3. OTLP endpoint: `otlp.tls.client_certs: required | optional`, CA from the
   server, emitter from the verified URI SAN, epoch check per request.
4. Boop Chat's emitter: `--otlp-ca-file`, `--otlp-cert-file`,
   `--otlp-key-file`, re-read when they change; then Jump and Core.
