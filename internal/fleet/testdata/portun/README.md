# Portun observability contract v1: test data

Copied from Portun Jump's repository (`testdata/o11y/v1/`, contract
`docs/observability.md`, at commit 9fa0a5c, after Boop became Portun and
every name went from `boop_*` / `boop.*` to `portun_*` / `portun.*`),
which publishes them for consumers' tests. `contract-metrics.txt` lists every metric name the
contract defines, existing or new; the integrations `portun`,
`portun_jump_gateway` and `portun_jump_agent` may use only those.

Shared values: tenant CA `SHA256:38lMZN9s593n4PAe0oPt/XESnoQduAWmRgcoOOzgK00`,
a second (rogue) CA `SHA256:IobRTOLc8fbef8bQ/ATQdXtuxHF2U1UDT2SchwG39ok`.
`TEST_EXPECT` in `sshd-journal.jsonl`: `jump`, `shadow:<why>`,
`ignore:<why>`; `shadow:serial_mismatch` is `jump` for a check that does
not recompute the serial, which a log rule cannot.

All keys behind these lines were throwaway keys.
