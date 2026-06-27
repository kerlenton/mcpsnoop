# Contributing to mcpsnoop

Thanks for taking the time to help. Bug reports, feature ideas, and pull
requests are all welcome.

## Getting started

You need Go 1.26 or newer.

```bash
git clone https://github.com/kerlenton/mcpsnoop && cd mcpsnoop
make build        # builds ./mcpsnoop
./mcpsnoop        # runs the TUI
```

To see real traffic while hacking, wrap a published server and drive it with a
real client — see [docs/DEMO.md](docs/DEMO.md).

## Before you open a pull request

Run the full gate. It must be green:

```bash
make check
```

That runs `gofmt -s`, `go vet`, [staticcheck](https://staticcheck.dev), and the
test suite under the race detector — the same checks CI runs. A focused unit
test for new behaviour is appreciated; most packages already have one to copy
the style from.

## Code style

- Idiomatic, modern Go: `any` over `interface{}`, the `slices`/`cmp` helpers,
  built-in `min`/`max`. staticcheck enforces a lot of this.
- Keep packages small and single-purpose. The proxy never interprets traffic;
  the store does all correlation; the TUI only renders snapshots. Please keep
  those seams clean.
- The transparency contract in `internal/proxy` is load-bearing: observation is
  best-effort and must never block, reorder, or alter the proxied bytes. Treat
  it as invariant.
- Comments explain *why*, not *what*. Match the surrounding tone.

## Commits and pull requests

- Work on a branch and open a PR against `main`; CI runs on every PR.
- One logical change per PR. Smaller is easier to review and merge.
- Write commit subjects as [Conventional Commits](https://www.conventionalcommits.org):
  `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, `ci:`. The release
  changelog is generated from these, and the prefix signals whether a change is
  a new feature or a fix (see Versioning).

## Versioning

mcpsnoop follows [Semantic Versioning](https://semver.org) (`vMAJOR.MINOR.PATCH`),
with the usual pre-1.0 rules:

- **While `0.x`** the tool is still stabilising. A **minor** bump (`0.Y.0`) may
  change user-facing behaviour — CLI flags, keybindings, the on-disk log format,
  or the shim↔hub protocol. A **patch** bump (`0.y.Z`) is reserved for bug fixes
  and backward-compatible additions.
- **From `1.0.0` on**, breaking changes go in a **major** bump, new features in a
  **minor**, and fixes in a **patch**.

A release is cut by pushing a `vX.Y.Z` tag; see [RELEASING.md](RELEASING.md).

## Reporting bugs and proposing features

Open an [issue](https://github.com/kerlenton/mcpsnoop/issues). For a bug, the
most useful thing you can include is the exact server command you wrapped and
the relevant frames (`y` copies a frame's JSON straight from the TUI).

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT License](LICENSE).
