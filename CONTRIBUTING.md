# Contributing

Thanks for helping with QTC. Problem reports, ideas and pull requests are all welcome. QTC is in an invite-only test, so reports from testers are the most valuable contribution right now.

## Issues first, or with your PR

The most useful thing you can send is a clear description of what should change and why. A good description usually lets me fix the problem even without a patch, and it's what I look at first when a pull request comes in.

For problems, please use the [problem report form](https://github.com/jancona/qtc/issues/new?template=bug_report.yml). It asks for what you did and what happened, your versions, your radio, the node status and logs. The [operator guide](docs/operator-guide.md) explains how to collect them. Messages are not private, so check what you paste for anything you'd rather not publish.

For ideas and other changes, open an issue describing:

- **What you're trying to do**, and what gets in the way today.
- **What you'd like to happen instead.**
- **Links to the relevant spec sections or code**, and a sketch of how you would do it, if you have one.

There's no such thing as too much detail.

## Pull requests

I treat a pull request as a detailed proposal. I may merge it as it is, add commits to it, or write the change myself from your description and close the PR. When your report or code leads to a change, you'll get credit in the commit.

What helps a pull request along:

- **Say how you tested it:** unit tests, a hotspot, a public station, `qtc chat`, or a radio (which one, and its firmware).
- **Keep it to one change.** Separate fixes are easier to review and test as separate PRs.
- **Run `gofmt`, `go vet`, `staticcheck` and `go test ./...`.**
- **Update the docs** when behavior or settings change.

## Design and wire formats

QTC's design is written down in [docs/](docs/), starting with [qtc-architecture.md](docs/qtc-architecture.md). Several decisions were made deliberately, and their reasoning is in those documents. Examples are derived message IDs, declared mailboxes, and no encryption. Please read the relevant document before proposing to change one of them.

Anything that changes what goes over the air or between nodes needs an issue before code. That covers the envelope, packet kinds, signatures, and the node protocol. Wire formats are tied to the specs and to the test vectors in `docs/qtc-fixtures.json`, which come from an independent reference generator. A change has to update the spec, the generator and the Go code together.

Protocol-level M17 changes (framing, addressing, M17_inet) belong in [m17](https://github.com/jancona/m17), not here.

## Questions

For questions, requests to join the test, and general discussion, find N1ADJ on the [M17 Project Discord](https://discord.gg/4brEP8wwVp).
