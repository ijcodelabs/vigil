# Runtime events

With runtime events on, the agent sees what every process on the host and in
its containers does, checks it against rules right there, and can save a
**capture**: everything that happened from shortly before a detection until
shortly after.

## What is seen

| Event | When | Fields besides the common ones |
|---|---|---|
| `exec` | a program is started | `args`, `cmdline` |
| `exit` | a process ends | `exit_code`, `signal` |
| `connect` | an outbound TCP connection is attempted | `addr`, `port` |
| `listen` | a TCP socket starts listening | `addr`, `port` |
| `open` | a file under a watched path is opened | `path`, `write` |
| `setuid` | a process changes its effective user | `old_euid`, `new_euid` |

Common to all: `type`, `pid`, `ppid`, `uid`, `euid`, `comm` (short process
name), `exe` (program file), `parent_comm`, `parent_exe`, `container`,
`container_id`. `container` is empty for processes on the host itself, the
container's name with Docker, and `namespace/pod/container` on Kubernetes.

## Switching it on

In the agent's configuration, usually through a profile:

```yaml
security:
  enabled: true
  watch_paths: [/etc/shadow, /etc/sudoers, /etc/ssh/, /root/.ssh/]   # at most 16, absolute
  capture_before: 30s
  capture_after: 30s
  capture_cooldown: 5m          # per rule, between captures
  rules:
    - name: shell_in_container
      when: type == "exec" && container != "" && exe in ["/bin/sh", "/bin/bash"]
            && !(parent_comm in ["runc", "containerd-shim", "conmon"])
      severity: warning          # info, warning or critical
      summary: "{exe} started in {container} by {parent_comm}"
      capture: true
```

`configs/agent.example.yaml` has seven rules to start from.

### What it costs

Measured on a busy Docker host (18 cores, 41 containers, x86-64, Linux
6.12): about 28 events a second, none lost; the agent went from 5% to 8% of
one core, and the host's own CPU use did not change measurably. The probes
look at every system call but return at once for all but a handful.

### Requirements

Linux 5.8 or newer with BTF, and root: a host install as root, or a
privileged container with the host's process namespace. The agent reports
the fact `ebpf` as `true` where it can work, so a profile can select on it:

```yaml
select: ['ebpf="true"', 'env="prod"']
```

Where it cannot run, the agent says so once in its log, the Fleet page shows
the reason, and metrics and logs carry on.

## Writing rules

Two things real hosts teach quickly:

- **A path is as the process sees it.** `/etc/shadow` opened inside a
  container is that container's file, not the host's. Health checks do
  this constantly (`pg_isready` in a Postgres container, every few
  seconds). For rules about the host's own files add `container == ""`.
- **Start a rule without `capture`, look at what it catches for a day,**
  then narrow it. Detections are in Explore under `source:security`;
  `source:security | stats by (rule, comm, container) count()` shows who
  trips what.

`when` is a [CEL](https://cel.dev) expression over the fields above. It must
be true or false.

| To say | Write |
|---|---|
| equals | `comm == "nc"` |
| one of | `comm in ["curl", "wget"]` |
| starts with, ends with, contains | `path.startsWith("/etc/")`, `exe.endsWith("/sh")`, `cmdline.contains("base64")` |
| regular expression | `cmdline.matches("-e\\s+/bin/")` |
| and, or, not | `&&`, `\|\|`, `!` |
| numbers | `port < 1024`, `new_euid == 0 && old_euid != 0` |
| in a container | `container != ""` |

`summary` is text with `{field}` placeholders. A rule reports at most
`max_per_minute` detections (default 10), so one noisy rule cannot flood.

### Trying rules out

```bash
sudo vigil events                              # every event, as JSON lines
sudo vigil events -config /etc/vigil/agent.yaml   # plus which rules fire
```

Run something, look at the event it produced, write the condition from its
fields.

## What happens on a detection

1. It is stored as a log line: `source:security` in [Explore](explore.md),
   with `rule`, `severity` and the event's fields.
2. It is raised as an [alert](alerts.md) at once, named after the rule,
   with labels `agent`, `severity` and `container`. Route it like any other.
3. If the rule says `capture: true`, a capture is saved.

## Captures

The agent keeps the recent past of all events in memory (the flight
recorder). When a capturing rule fires, it saves everything from
`capture_before` earlier to `capture_after` later and uploads it.

On the **Captures** page a capture shows as a timeline relative to the
detected event: what ran, what was opened, where connections went. Filter by
text or by event type; click a row for every field; download it as JSON.

- A host records one capture at a time. Detections during a capture are
  linked to it.
- The server keeps captures up to `captures_max_bytes` (2 GiB by default)
  and removes the oldest first.

## Limits to know

- **File opens are matched on absolute paths.** An open relative to a
  directory (`cd /etc && cat shadow`) is not seen.
- **Network events are TCP only**: connections made and sockets listening.
  Accepted connections, UDP and DNS are not reported.
- Every system call passes a small check in the kernel. The cost has not
  been measured on a busy host.
- Loaded so far only on arm64 with kernel 7.1. The x86-64 build of the probe
  exists but has not run; there is no fallback below Linux 5.8.
- If the host produces events faster than the agent reads them, some are
  dropped and counted in `vigil_events_lost_total`.
