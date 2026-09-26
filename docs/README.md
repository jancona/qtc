# QTC documentation

Running QTC: [`operator-guide.md`](operator-guide.md) for hotspot and station operators; [`user-guide.md`](user-guide.md) for radio users.

Design, for developers. Read in this order:

1. `qtc-architecture.md` — roles, trust, identity, message flow, milestones
2. `qtc-envelope.md` — MSG/RCPT packet types
3. `qtc-rooms.md` — rooms and the ROOM packet type
4. `qtc-node-protocol.md` — node-to-node protocol on libp2p (draft 0.2)

`qtc-node-protocol-draft01-custody.md` is the superseded custody-based design, kept for history.

`qtc-fixtures.json` holds the test vectors; `qtc-fixtures-gen.py` is the Python reference implementation that generates them (`pip install cryptography`, then run it).
