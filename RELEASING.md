# Releasing

A release is just a pushed tag. Tag a commit on `main` and GitHub Actions takes
it from there:

```bash
git switch main && git pull
git tag v0.1.0
git push origin v0.1.0
```

The [release workflow](.github/workflows/release.yml) runs GoReleaser: it
cross-compiles the binaries, builds the archives and `checksums.txt`, and
publishes a GitHub Release. The only secret it needs is the default
`GITHUB_TOKEN`.

Pick the version with [SemVer](https://semver.org) (full policy in
[CONTRIBUTING](CONTRIBUTING.md#versioning)): while on `0.x`, `0.Y.0` may change
behaviour and `0.y.Z` is bug fixes only.
