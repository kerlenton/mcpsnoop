# Releasing

A release is just a pushed tag. Tag a commit on `main` and GitHub Actions takes
over from there.

```bash
git switch main && git pull
git tag v0.2.0
git push origin v0.2.0
```

The [release workflow](.github/workflows/release.yml) runs GoReleaser. It
cross-compiles the binaries, builds the archives and `checksums.txt`, and
publishes a GitHub Release. The only secret it needs is the default
`GITHUB_TOKEN`.

Pick the version with [SemVer](https://semver.org), and see the full policy in
[CONTRIBUTING](CONTRIBUTING.md#versioning). While on `0.x`, a `0.Y.0` bump may
change behaviour and a `0.y.Z` bump is bug fixes only.
