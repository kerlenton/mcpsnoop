# Releasing mcpsnoop

Releases are cut by pushing a `vX.Y.Z` tag. The
[`release`](.github/workflows/release.yml) workflow runs GoReleaser, which
cross-compiles the binaries, builds the archives and checksums, and publishes a
GitHub Release. No secrets beyond the default `GITHUB_TOKEN` are required.

## Cut a release

```bash
git switch main && git pull
git tag v0.1.0
git push origin v0.1.0
```

Then check the **Actions** tab (the `release` job is green) and the **Releases**
page (archives for linux/darwin/windows × amd64/arm64 plus `checksums.txt`).

## Dry run (optional)

With [GoReleaser](https://goreleaser.com) installed:

```bash
goreleaser release --snapshot --clean   # builds into ./dist, publishes nothing
```

## Install paths after a release

```bash
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
# or download a prebuilt binary from the Releases page
```

Package managers (Homebrew, etc.) are planned but not wired up yet.
