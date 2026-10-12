# Decisions

**Storage is VictoriaMetrics and VictoriaLogs**, behind the server's own
ingest and query API. Writing a time-series or log database was ruled out
at the start. Agents never talk to the stores; the server stamps every
series and log entry with `agent=<id>`, so an agent cannot write as another.

**One static binary, no cgo.** It must run on Debian, RHEL and Ubuntu
without packaging. That is why journald is read by running `journalctl`,
SQLite is the pure-Go `modernc.org/sqlite`, and Docker is spoken to over its
socket directly.

**Generated artefacts are committed.** The eBPF objects
(`internal/events/events_*_bpfel.o`) and the two vendored UI libraries, so
that `go build` needs nothing else. Alertmanager's `app` package taught the
lesson: it cannot be imported because its web assets are not in the module.

**Alertmanager is wired from its internal packages**, following its own
`app/app.go`, without clustering or UI. Pinned to v0.34.1; re-check the
wiring in `internal/alerting` on upgrade.

**The UI has no build step.** Plain ES modules embedded with `go:embed`,
plus uPlot and Preact+htm vendored as single files.

**Rule files and the alerting config are whole documents**, replaced on
save and validated first (every expression is run once against its store).

**Security rules run on the agent**, not the server, because a capture must
trigger in milliseconds and work while the server is unreachable. They are
CEL expressions.

**System-call probes are raw tracepoints** (`tp_btf/sys_enter`,
`sys_exit`). The per-call `syscalls:sys_enter_*` tracepoints, attached
through perf, were silently not called for many processes on kernel 7.1
arm64. See [testing.md](testing.md).

**No reduced mode for old kernels.** Runtime events need 5.8 and BTF;
elsewhere the agent says so and carries on. A fallback was not written
because there was no old kernel to test it on.

**One config format everywhere.** A profile's config fragment uses the same
schema as an agent's local file; lists of jobs, log sources and security
rules merge by their name, and `enabled: false` switches an inherited item
off. Two schemas (maps on the server, lists on the agent) were considered
and rejected as more to learn.

**Config can be edited on the server or come from files in git.** Each
profile records where it was last written from.

**Some rules are built in, and the rule file overrides them by alert name.**
A new server has an empty rule file, and "is it down" should not depend on
someone uploading an example. The rule file is what people keep in git, so
it has the last word: it then holds only what differs from the defaults,
which change with the version. Overriding by group name alone was rejected
because changing one threshold would mean copying the whole group.

**A stopped container alerts only with a non-zero exit code.** Exit 0 is a
job that finished; a removed container is gone on purpose. A container
stopped by hand usually ends with 137 or 143 and does alert, which was
accepted: it is down, and a silence covers planned work.
