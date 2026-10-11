# vigil

A monitoring system in one Go binary: metrics, logs, alerting, runtime
security events and dashboards. `README.md` describes what it does and how to
run it.

Before changing anything, read `memory/context/`: `state.md` (what is built
and what is in progress), `decisions.md` (why), `direction.md` (where it is
going) and `testing.md` (how to run it for real). Update those files when
something they say stops being true.

- Build: `make build`; `make linux` for static Linux binaries; `make test`.
- After changing `internal/events/bpf/events.c`: `make bpf`, and commit the
  regenerated `events_*_bpfel.{go,o}`.
- Code style: `gofmt`, comments that say why, tests next to the code.
- UI changes are checked in a browser against a running server.
- User-facing behaviour is documented in `docs/` (shown by the server under `/docs/`); update the page when the behaviour changes.
