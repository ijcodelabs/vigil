# Runtime events

With runtime events on, the agent sees what every process on the host and in
its containers does, checks it against rules right there, and can save a
**capture**: everything that happened from shortly before a detection until
shortly after.

## What is seen

| Event | When | Fields besides the common ones |
|---|---|---|
| `exec` | a program is started | `args`, `cmdline` |
| `exit` | a process ends (once, when its last thread ends) | `exit_code`, `signal` |
| `connect` | an outbound TCP connection is attempted | `addr`, `port` |
| `listen` | a TCP socket starts listening | `addr`, `port` |
| `open` | a file under a watched path is opened, or the kernel refused to open it | `path`, `write`, `refused` |
| `setuid` | a process's effective user changes: a set*uid call, running a setuid program (`sudo`, `su`, …), or from inside the kernel | `old_euid`, `new_euid` |

`path` is the file the kernel opened, whatever name the program gave:
`cd /etc && cat shadow`, `/etc/../etc/shadow` and a symlink to it all report
`/etc/shadow`. Inside a container it is the path as the container sees it.
`refused` is true when the kernel denied the open for lack of permission:
a user outside the `shadow` group running `cat /etc/shadow`, or a write to
`/etc/sudoers` by someone other than root. Such an open has no file behind
it, so `path` is the name the program gave, and only an absolute name is
matched. An open of a name that does not exist is not reported. A rule on
a path sees both kinds; add `!refused` for what was actually opened, or
`refused` for attempts only.
For a setuid program the `setuid` event comes with the old program's
`comm`; the `exec` event of the new one follows.

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
run on every file open, and resolve its path before comparing it with
`watch_paths`; on an arm64 test machine (Linux 6.8) that added about 65 ns
to an open of 700 ns.

### Requirements

Linux 5.8 or newer with BTF, and root: a host install as root, or a
privileged container with the host's process namespace. Opens and changes
of user are watched where the kernel can attach BPF programs to kernel
functions (fentry: x86-64 from Linux 5.5, arm64 from 6.0; no BPF security
module needed). Where it cannot, the agent uses its system-call probes
instead and logs `fentry probes unavailable` with the reason: they see
only opens by absolute path and only the set*uid calls, and they report
every attempt to open a watched name without knowing whether it was
refused (`refused` is always false there).
`VIGIL_EVENTS_PROBES=syscalls` in the agent's environment chooses those
probes anyway. The agent reports
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
sudo vigil-agent events                              # every event, as JSON lines
sudo vigil-agent events -config /etc/vigil/agent.yaml   # plus which rules fire
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
Every event a rule fired on during the capture is marked **detected**, not
only the first. The list next to the filter narrows it to **Detected** (those
events) or **Errors** (opens the kernel refused, and processes that ended
with an error code or a signal), whatever event types are ticked.

- A host records one capture at a time. Detections during a capture are
  linked to it.
- The server keeps captures up to `captures_max_bytes` (2 GiB by default)
  and removes the oldest first.

## Limits to know

- **With the system-call probes** (older kernels, see Requirements), file
  opens are matched on the name as given: an open relative to a directory
  (`cd /etc && cat shadow`), through `..` or through a symlink is not seen,
  and running a setuid program is not a `setuid` event. Every attempt is
  reported as an open, refused or not, also of a name that does not exist.
- **Network events are TCP only**: connections made and sockets listening.
  Accepted connections, UDP and DNS are not reported.
- The open probe resolves the path of every open on the host. Measured on
  a test machine, not yet on a busy host.
- Run so far on arm64 (Linux 6.8 and 7.1) and x86-64 (Linux 6.8, and 6.12
  before the fentry probes). There is no fallback below Linux 5.8.
- If the host produces events faster than the agent reads them, some are
  dropped and counted in `vigil_events_lost_total`.
