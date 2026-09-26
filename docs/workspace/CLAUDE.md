# Workspace: qtc + m17

Two repos, one Go workspace (`go.work`).

- `m17/` — M17 protocol library: framing, address encoding, M17_inet. Protocol-level changes go here.
- `qtc/` — QTC messaging system, which depends on `m17`. See `qtc/CLAUDE.md`.

Default to `qtc/` for any change; touch `m17/` only for things the M17 specification defines. When a change needs both, do the `m17` side first and say so.
