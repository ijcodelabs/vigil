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
| `namespace` | a process joins other namespaces (`setns`, `nsenter`) or makes new ones (`unshare`) | `namespaces` (those that changed: `mnt`, `uts`, `ipc`, `pid`, `net`, `cgroup`), `host_namespaces` (of those, the ones that are now the host's), `to_host` |
| `mount` | something is mounted with `mount(2)` | `source`, `fstype`, `target` (the mount point's last two parts, as `.../host/etc`), `bind`, `remount` |
| `ptrace` | a process attaches to another with ptrace (a debugger, `strace`, or code injection) | `target_pid`, `target_comm` |
| `module` | a kernel module is loaded | `module` |
| `bpf` | a BPF program is loaded into the kernel | `prog_name`, `prog_type` |
| `connect` with `unix` | a connection to a container runtime's socket (a name ending in `docker.sock`, `containerd.sock`, `crio.sock` or `podman.sock`) | `addr` is the socket's path, `unix` is true |
| `container` | the agent sees a container running for the first time: when it starts, and for every running container when the agent starts | see [Container setup](#container-setup) |

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
  watch_paths: [/srv/app/config/]   # yours, besides the built-in rules' files; at most 16 in all
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

With `enabled: true` the [built-in rules](#built-in-rules) are on as well;
`configs/agent.example.yaml` shows how to replace one and add your own.

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

## Built-in rules

The agent ships with the rules below. They are on whenever runtime events
are, and the files they look at are added to `watch_paths` (your paths
first; at most 16 in all, and the agent logs any it had to leave out).

```yaml
security:
  enabled: true
  disable_rules: [package_manager_in_container]   # leave some out
  # builtin_rules: false                          # or all of them
  rules:
    - name: shell_in_container    # the same name replaces the built-in rule
      when: ...
      capture: true
```

None saves a capture: see what a rule catches on your hosts for a day,
then replace it with your own version, narrowed and with `capture: true`.

| Rule | Severity | Fires when |
|---|---|---|
| `privileged_container` | warning | a container runs privileged |
| `host_namespace_container` | warning | a container shares the host's processes or IPC |
| `runtime_socket_in_container` | warning | a container has a runtime's socket mounted |
| `dangerous_capability` | warning | a container was given `ALL`, `SYS_ADMIN`, `SYS_PTRACE`, `SYS_MODULE`, `SYS_RAWIO`, `DAC_READ_SEARCH` or `BPF` |
| `sensitive_host_mount` | warning | a container has `/`, `/etc`, `/root`, `/home`, `/boot`, `/dev`, `/proc`, `/sys` or a runtime's data directory mounted |
| `sensitive_host_mount_writable` | critical | the same, writable |
| `system_config_changed` | critical | a file under `/etc/sudoers*`, `/etc/ssh/`, `/root/.ssh/` or `/etc/ld.so.preload` is opened for writing on the host, not by a package manager |
| `scheduled_task_changed` | warning | the same for `/etc/cron*`, `/var/spool/cron/` and `/etc/systemd/system/` |
| `password_file_changed` | critical | `/etc/shadow` is written by something other than the account tools or a package manager |
| `user_account_changed` | info | `useradd`, `usermod`, `passwd` and the like run on the host |
| `sensitive_file_read` | critical | `/etc/shadow` is opened on the host by anything but the login and account programs and package configuration |
| `became_root` | critical | a process whose real user is not root becomes root other than through `sudo`, `su`, `sshd`, `cron`, `login` or `pkexec` (a root process taking root back after dropping it for a while, as apt does, is left out) |
| `run_from_temp_dir` | warning | a program runs from `/tmp`, `/var/tmp` or `/dev/shm` |
| `unexpected_listener` | warning | something other than the usual servers listens on a port below 1024 |
| `shell_in_container` | warning | a program in a container starts a shell: a web app running one is the classic sign of a break-in (`docker exec`, health checks, the container's first process, and jobs of `cron`, `anacron` and `logrotate` are left out, as is `pg_dumpall` asking `pg_dump` for its version) |
| `download_tool_in_container` | info | `curl`, `wget`, `nc`, `ncat` or `socat` runs in a container, other than towards `localhost` (health checks) |
| `container_joined_host_namespace` | critical | a process in a container joins one of the host's namespaces (`nsenter -t 1`): it has left its container |
| `mount_in_container` | warning | a process in a container mounts something (the runtime's own mounts left out). Storage drivers in privileged pods do this all the time: turn it off on such nodes |
| `ptrace_in_container` | critical | a process in a container attaches to another with ptrace |
| `ptrace_of_system_process` | warning | on the host, something attaches to `sshd`, `sudo`, `su`, `login`, `systemd`, `dockerd`, `containerd` or the agent: it can read their memory, passwords included |
| `module_loaded` | info | a kernel module is loaded on the host (at boot and when Docker starts, too) |
| `module_from_container` | critical | a process in a container loads a kernel module |
| `bpf_from_container` | critical | a process in a container loads a BPF program into the host's kernel |
| `runtime_socket_used_in_container` | critical | a process in a container talks to the container runtime's socket. Tools that are meant to (Traefik, Portainer) do this too: replace the rule with one that leaves them out |
| `package_manager_in_container` | info | `apt`, `apk`, `yum`, `dnf`, `pip` and the like run in a container; also what `docker build` does, so it is noisy on build hosts. Questions that change nothing, as a daily apt job inside a container asks them (`dpkg --print-architecture`, `--print-foreign-architectures`, `apt-get check`), are left out |

The container rules leave out the vigil agent's own image, which needs the
host's processes and root to do its work. `authorized_keys` in users' home
folders is not watched: a prefix of `/home/` would report every open
there. Add `/home/USER/.ssh/` to `watch_paths` for the accounts that
matter; `system_config_changed` does not look there, so add a rule too.

## Container setup

A `container` event says how a container was started. It comes from Docker
(with the agent's [Docker settings](configs.md#containers-on-a-docker-or-podman-host) on) or from the node's pod list
on Kubernetes, not from the kernel, so it also works where the probes cannot
run: there the agent logs that runtime events are off and keeps checking
containers. It costs one question to Docker per new container; on
Kubernetes the pod list is already there.

| Field | True or set when |
|---|---|
| `privileged` | the container runs privileged |
| `host_pid`, `host_network`, `host_ipc` | it shares the host's processes, network or IPC |
| `cap_add` | capabilities added to the default set, as `SYS_ADMIN` (without `CAP_`) |
| `host_mounts`, `host_mounts_rw` | host paths mounted into it; the second list holds only the writable ones |
| `runtime_socket` | a container runtime's socket (Docker, containerd, CRI-O, Podman), or the directory holding it such as `/var/run`, is mounted: whoever reaches it can start a privileged container |
| `runs_as_root` | it runs as root |
| `read_only_root` | its own filesystem is read-only |
| `image` | the image it runs |

`container` and `container_id` are set as for the other events; the process
fields are empty. A rule without a `summary` says
"`shop-api` started from `acme/api:1`".

```yaml
- name: host_root_mounted
  when: type == "container" && host_mounts.exists(m, m == "/" || m == "/etc")
  severity: critical
  summary: "{container} has the host's {host_mounts} mounted"
```

On Kubernetes `runs_as_root` is read from the pod spec: true unless
`runAsNonRoot` or a non-zero `runAsUser` is set, because the image's own
user is not in the spec. With Docker it is the user the container really
runs as.

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

- **The escape probes** (`namespace`, `mount`, `ptrace`, `module`, `bpf`,
  runtime sockets) are each loaded on their own; a kernel that lacks one's
  function goes without that one, says so in the agent's log, and the
  metric `vigil_events_probe_up{probe="..."}` is 0 for it. All but
  `module` need the fentry probes (see Requirements). Tested on Linux 6.8
  (arm64) and 6.12 (x86-64).
- **Mounts** made with the newer `fsopen`/`fsmount` calls are not seen, and
  a mount point is given by its last two path components only.
- **A container that lives less than about ten seconds** (`docker run
  --rm` of a quick command) may get no `container` event: the agent asks
  the runtime every ten seconds. Its processes are still seen.
- **Runtime sockets** are recognised by name: a socket mounted into a
  container under another name is not.

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
