# vigil

A monitoring system in a few static Go binaries (`vigil-agent`,
`vigil-server`, `vigil-relay`, and `vigil` for the operator tools): metrics,
logs, alerting, runtime security events and dashboards. `README.md`
describes what it does and how to run it.

Before changing anything, read `memory/context/`: `state.md` (what is built
and what is in progress), `decisions.md` (why), `direction.md` (where it is
going) and `testing.md` (how to run it for real). Update those files when
something they say stops being true.

- Build: `make build`; `make linux` for static Linux binaries; `make test`;
  `make size` for the agent's and relay's size budgets. The agent and relay
  must not import server code (`internal/depcheck`).
- After changing `internal/events/bpf/events.c`: `make bpf`, and commit the
  regenerated `events_*_bpfel.{go,o}`.
- Releases: tags `vX.Y.Z` on `main`, built by `.github/workflows/release.yml`;
  a user-visible change adds a line under **Unreleased** in `CHANGELOG.md`.
  See `memory/context/releasing.md`.
- Code style: `gofmt`, comments that say why, tests next to the code.
- UI changes are checked in a browser against a running server.
- User-facing behaviour is documented in `docs/` (shown by the server under `/docs/`); update the page when the behaviour changes.

## What does not belong in this repository

The repository is public, and so are its notes in `memory/context/`.

- No key, token or password, real or left over from a test. Examples use
  placeholders and `*_file` settings; CI scans every commit (gitleaks).
- Nothing that names a real deployment: host names, addresses, domains,
  people, clients. Write "the trial host", `shop-api`, `example.com`, and
  the documentation address ranges.
- No security weakness that is known and not fixed yet. It is fixed first
  or reported the way `SECURITY.md` describes; a note about it comes after.
- What is built, why it was built that way, what is not built and what is
  planned are welcome: that is what lets someone else continue.

Whoever runs vigil somewhere keeps the notes about that deployment with the
deployment, not here.

