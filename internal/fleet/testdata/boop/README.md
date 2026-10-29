# Boop observability contract v1: test data

Copied from Boop Jump's repository (`testdata/o11y/v1/`, contract
`docs/observability.md`, at commit 3bd30d9), which publishes them for
consumers' tests. `contract-metrics.txt` lists every metric name the
contract defines, existing or new; the integrations `boop`,
`boop_jump_gateway` and `boop_jump_agent` may use only those.

Shared values: tenant CA `SHA256:38lMZN9s593n4PAe0oPt/XESnoQduAWmRgcoOOzgK00`,
a second (rogue) CA `SHA256:IobRTOLc8fbef8bQ/ATQdXtuxHF2U1UDT2SchwG39ok`.
`TEST_EXPECT` in `sshd-journal.jsonl`: `jump`, `shadow:<why>`,
`ignore:<why>`; `shadow:serial_mismatch` is `jump` for a check that does
not recompute the serial, which a log rule cannot.

All keys behind these lines were throwaway keys.
