# Releasing mcpsnoop

Release tooling is ready (`.goreleaser.yaml`, `.github/workflows/release.yml`).
The config is validated — `goreleaser check` passes schema validation; it only
reports "not a git repository" until the steps below create one.

## One-time setup

1. Create the GitHub repos under your account:
   - `kerlenton/mcpsnoop` (this project)
   - `kerlenton/homebrew-tap` (empty; GoReleaser pushes the formula here)
2. Create a Personal Access Token with `contents:write` on `homebrew-tap` and add
   it to `kerlenton/mcpsnoop` as the secret **`HOMEBREW_TAP_GITHUB_TOKEN`**.
   (The release workflow already wires `GITHUB_TOKEN` for the main repo.)

## Cutting a release (the single commit)

```bash
git init
git add -A
git commit -m "mcpsnoop v0.1.0"        # the one and only initial commit
git branch -M main
git remote add origin git@github.com:kerlenton/mcpsnoop.git
git push -u origin main

git tag v0.1.0
git push origin v0.1.0                  # triggers .github/workflows/release.yml
```

The tag push runs GoReleaser, which builds binaries for
linux/darwin/windows × amd64/arm64, publishes a GitHub Release with archives +
`checksums.txt`, and updates the Homebrew formula in `kerlenton/homebrew-tap`.

## Dry run (optional, needs goreleaser installed)

```bash
goreleaser release --snapshot --clean   # builds into ./dist without publishing
```

## After release

Install paths in the README work once the above completes:

```bash
brew install kerlenton/tap/mcpsnoop
go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@latest
```
