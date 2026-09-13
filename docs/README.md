# Pigeon documentation

Read in this order:

1. `pigeon-architecture.md` — roles, trust, identity, message flow, milestones
2. `pigeon-envelope.md` — MSG/RCPT packet types
3. `pigeon-rooms.md` — rooms and the ROOM packet type
4. `pigeon-node-protocol.md` — node-to-node protocol on libp2p (draft 0.2)

`pigeon-node-protocol-draft01-custody.md` is the superseded custody-based design, kept for history.

`pigeon-fixtures.json` holds the test vectors; `pigeon-fixtures-gen.py` is the Python reference implementation that generates them (`pip install cryptography`, then run it).
