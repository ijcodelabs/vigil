# Security

vigil takes sign-ins, holds agent tokens and loads eBPF programs into the
kernel, so reports about it are welcome and taken seriously.

## Reporting a vulnerability

Report it privately: on GitHub, **Security → Report a vulnerability** in this
repository. Do not open a public issue for something that can be exploited.

Say what you did, what happened, and which version or commit it was
(`vigil version`). A request, a config or a few lines that reproduce it help
more than a description.

You get an answer within a week. A fix comes with a new release and a note
that names you, unless you would rather not be named.

## Supported versions

Fixes go into the latest release only. There are no maintenance branches.

## What counts

- Reaching the server's data, API or configuration without credentials, or
  with a viewer account what only an admin may do.
- One agent's token giving access to another agent's data or config.
- The agent or its eBPF probes letting an unprivileged local user gain
  privileges, or crashing the host.
- Credentials leaking into logs, captures, metrics or the web interface.

What an admin can do is not a vulnerability: an admin decides what agents
run and collect. Neither is anything that needs root on the host first.
