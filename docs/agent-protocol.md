# Sending from your own software

`vigil-agent` is one way to get data into vigil, not the only one. Any
program can enrol with the server, push metrics and logs, and show up on
the Fleet page like an agent: a hardware monitor on a Windows gaming PC, an
appliance, a device that cannot run the agent. This page is the protocol
such a program speaks. It is the same one `vigil-agent` uses.

If all you have is a Prometheus `/metrics` page, you do not need any of
this: let an agent nearby scrape it ([Agent configuration](configs.md)).
Speaking the protocol is for when the program should be a host of its own
on the Fleet page, with its own token, and keep sending when nothing on its
network runs an agent.

## What the user gives your program

- the **server address**, e.g. `https://vigil.example.com`;
- an **enrolment token** (`vge_…`), made under **Fleet → Enrolment**;
- for a server with its own certificate authority, its **fingerprint**
  (`sha256:…`), shown on the same page (see [TLS](#tls));
- the **agent id** it should have, by default the computer's name.
  Letters, digits, `.`, `_` and `-`, at most 128 characters.

## 1. Enrol, once

```
POST /api/v1/enroll
{"token": "vge_…", "agent_id": "gaming-pc"}
→ 200 {"agent_id": "gaming-pc", "token": "vgl_…"}
```

The answer's token is the program's own. Keep it the way the system keeps
secrets (on Windows with DPAPI, on macOS in the keychain), not in a plain
settings file. The enrolment token is not needed any more.

`403` means the enrolment token is wrong, expired or used up; repeated
failures from one address are slowed down like sign-in attempts.

## 2. Every request after that

```
Authorization: Bearer vgl_…
X-Vigil-Agent: gaming-pc
X-Vigil-Version: gamemon/0.2.0
```

The agent id must be the one the token was issued for, or the server
answers `403`. `X-Vigil-Version` is shown on the Fleet page; use
`<program>/<version>`.

## 3. Metrics

```
POST /api/v1/write
Content-Type: application/x-protobuf
Content-Encoding: snappy
X-Prometheus-Remote-Write-Version: 0.1.0
```

The body is a Prometheus remote-write v1 `WriteRequest`, snappy
block-compressed: any remote-write library produces it. Timestamps are in
milliseconds. Send a batch every 10 to 30 seconds with all samples since
the last one; samples taken every second or every five are fine inside a
batch.

The server adds `agent="<id>"` to every series; leave that label out. Add
the labels the user gave (`env`, `room`, …) to every series yourself.

**Use vigil's metric names where they fit**, so the Hosts, GPUs and other
dashboards and rules work for your program's hosts too:

| What | Names and labels |
|---|---|
| memory | `host_memory_total_bytes`, `host_memory_used_bytes`, `host_memory_available_bytes` |
| filesystems | `host_filesystem_size_bytes`, `host_filesystem_free_bytes` with `mountpoint`, `device`, `fstype` |
| disk I/O | counters `host_disk_read_bytes_total`, `host_disk_written_bytes_total`, `host_disk_reads_total`, `host_disk_writes_total` with `device` |
| CPU temperature | `host_cpu_temperature_celsius` with `cpu` (socket, from 0) and `sensor`: `package`, or `ccd1`, `ccd2`… for AMD's core dies |
| GPUs | `gpu_utilization_ratio`, `gpu_memory_used_bytes`, `gpu_memory_total_bytes`, `gpu_temperature_celsius`, `gpu_power_watts`, `gpu_power_limit_watts`, `gpu_fan_ratio` with `gpu` (index) and `vendor` (`nvidia`, `amd`, `intel`); `gpu_info{gpu, vendor, model, uuid, driver} 1` |

The full list is in the [Metric reference](metrics.md). Counters are
counters (`_total`, growing since the program or the computer started),
not rates: the server's queries take the rate. Ratios are 0 to 1, not
percent. Everything else gets your program's own prefix
(`gamemon_fps`, `gamemon_gpu_hotspot_celsius`).

## 4. Logs and events

```
POST /api/v1/logs
```

The body is gzip-compressed newline-delimited JSON, one line per entry:

```json
{"t": 1764000000123000000, "m": "Game session: Example Game, 62 min, 92 fps on average", "s": {"source": "gamemon", "kind": "game_session"}, "f": {"level": "info", "game": "Example Game", "duration_s": "3720", "avg_fps": "92.4"}}
```

| Key | |
|---|---|
| `t` | time, Unix **nanoseconds** |
| `m` | the line as a person reads it; Explore and log panels show this |
| `s` | stream labels: few, and with few values (`source`, `kind`). Not names of games, users or files |
| `f` | fields: anything else. **All values are strings** (`"92.4"`, not `92.4`); queries can still compare them as numbers |

Use `f.level` for the severity (`info`, `warning`, `err`, `crit`), as
journald lines do, and RFC 3339 for times inside fields. The server adds
`agent`; a field of that name is renamed `exported_agent`. `_msg`, `_time`
and `_stream` are reserved.

`POST /api/v1/events` is for runtime security detections only; events of
your program go to the logs.

## 5. Say what you are

```
POST /api/v1/agent/config
{"labels": {"room": "study"}, "facts": {"kind": "gamemon", "os": "windows", "distro": "Windows 11", "distro_version": "24H2", "arch": "amd64", "hostname": "GAMING-PC", "version": "0.2.0"}}
→ 204
```

Once at start and then every minute or so. The labels and facts appear on
the Fleet page and can be searched there; the facts are those of
[Fleet](fleet.md#labels-and-facts).

**`kind` is what makes it work.** Any value other than `vigil-agent` says
"I am not a vigil-agent": profiles never apply to it, even one that selects
every host, because a profile is vigil-agent configuration your program
would not understand. The server answers `204` without a body, the Fleet
page shows the kind next to the id, and its configuration as "not
managed". Without `kind`, a profile for all hosts would wait for ever for
your program to pick it up.

## 6. Being "up"

There is no separate heartbeat. Every accepted batch counts: an agent from
which nothing arrived for a minute (`heartbeat_timeout`) is shown as not
reporting, and `vigil_agent_up{agent="…"}` drops to 0 for the alert rules.
So keep sending while idle; a batch with only the current memory figures is
enough.

## 7. When the server says no

| Answer | Means | Do |
|---|---|---|
| `204` | stored | |
| `400` | the batch is malformed | log it, drop the batch |
| `401`, `403` | the token was revoked, or the id is not the token's | stop, tell the user to enrol again |
| `413` | the batch is too big (32 MB compressed, 64 MB of logs uncompressed) | send smaller batches |
| `429` | over this agent's limit (20 000 samples a second, 4 MB of logs a second, 10 000 different series an hour by default); `Retry-After` | keep the batch, send it again later |
| `503` | the server or a store is busy or down; `Retry-After` | keep the batch, send it again later |
| no answer | network, server restarting | keep the batch, send it again later |

Keep what could not be sent in a queue on disk with a cap (vigil-agent's is
512 MB), and send the oldest first when the server is back.

## TLS

- With a certificate from a public authority, use the system's trusted
  roots like any HTTPS client.
- A server with **its own certificate authority** (`tls: {self_signed:
  true}`) gives its fingerprint: `sha256:` and the hex SHA-256 of the
  authority's certificate (DER). Fetch the certificate once from
  `GET /api/v1/ca` (PEM, no token; skip verification for that one call
  only), compare its fingerprint with the one the user gave, refuse on a
  mismatch, keep the certificate, and from then on trust it as the only
  root for this server. Do not pin the server's own certificate: the server
  renews it.
- Plain `http://` sends the token in every request, readable by anyone on
  the network. Use it for a test only: allow it only to a private address
  (10/8, 172.16/12, 192.168/16, fd00::/8), checked on the address the name
  resolves to, not on the name, and say so in your program. Never fall
  back from HTTPS to HTTP, or from a fingerprint that does not match to
  accepting the server anyway.

## Security

A program that speaks this protocol holds a token that writes as one host,
and often runs with high privileges itself (a hardware monitor needs
them). Neither end should be a way into the other.

**The token**

- Make the enrolment token for one host, short-lived: under **Fleet →
  Enrolment**, *at most 1 use* and a few hours. It is only good for
  enrolling; afterwards it can be revoked.
- Keep the agent token encrypted for the user or machine (DPAPI,
  keychain), never in a log, a crash report or the settings window.
- Keep the server address inside the same protected store as the token,
  and refuse to send when the configured address no longer matches.
  Otherwise whoever can edit the settings file can point the program at
  their own host and receive the token.
- Do not follow redirects. A `3xx` is an error, never a reason to send the
  token or the data to another address.
- A stolen token is revoked under **Fleet → Agents → Manage**. It only
  ever wrote as that one agent: the server stamps every series and line
  with the token's own id.

**The program**

- Act on nothing the server returns beyond the status code, a short error
  text and the `Date` header: no configuration, no commands, no updates.
  Then a server that has been taken over cannot reach the hosts.
- A program running with administrator rights must not parse files that
  ordinary programs of the same user may write (settings, the queue on
  disk) with anything but strict, size-limited parsers, or better keep
  them where only administrators can write. Otherwise malware without
  those rights can plant a file the privileged program reads.
- Open no listening port, or one on 127.0.0.1 only.
- What leaves the computer (game and process names, file paths) is the
  user's to decide: send it only when they switched it on.

**What the server holds it to**

- It writes only as its own id; `agent` labels and fields are replaced.
- Lines it sends with `source="security"` are stored as
  `exported_source`: only runtime detections, through their own endpoint,
  appear on the security pages and on Home.
- `kind` is a short lower-case name, or the report is refused (`400`).
- The [limits](install.md#limits) on samples and log bytes per second
  apply as to any agent, and it may write at most 10 000 different series
  an hour (`other_kinds_series_per_hour`); a batch over that gets `429`
  with `Retry-After: 300`. Since a program decides itself whether it
  reports a kind, give an agent you do not trust a `series_per_hour` of its
  own; VictoriaMetrics' `-storage.maxHourlySeries` caps the whole store.

## An example: GameMon

GameMon, a hardware monitor and in-game overlay for Windows, speaks this
protocol. It reports `kind: gamemon`, uses the shared names above (CPU and
GPU temperatures every second, the rest every five), adds `gamemon_fps`,
`gamemon_frametime_ms`, `gamemon_gpu_hotspot_celsius`, `gamemon_gpu_throttle`,
SMART figures and more, and sends each game session and alert as a log
line. The **Gaming PCs** integration has the dashboard and alert rules for
it; switch it on with a profile that names it ([Fleet](fleet.md#integrations)):

```yaml
# profile "gaming-pcs": installs the dashboard and runs the rules. Profiles
# do not apply to GameMon itself, so the selector picks no host.
select: ['kind="gamemon"']
integrations:
  gaming_pc: {cpu_hot_celsius: "92"}
```
