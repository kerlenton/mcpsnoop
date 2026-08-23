# Releasing

A release is just a pushed tag. Tag a commit on `main` and GitHub Actions takes
over from there.

```bash
git switch main && git pull
git tag vX.Y.Z
git push origin vX.Y.Z
```

The [release workflow](.github/workflows/release.yml) runs GoReleaser. It
cross-compiles the binaries, builds the archives and `checksums.txt`, and
publishes a GitHub Release. That half needs only the default `GITHUB_TOKEN`.

## npm

A second job then publishes the same release to npm, so that
`npx mcpsnoop -- node build/index.js` works without a Go toolchain. This one
needs an `NPM_TOKEN` secret, a granular automation token with publish rights on
`mcpsnoop` and on the `@mcpsnoop` scope.

It downloads the archives the release just published, checks each one against the
`checksums.txt` published beside it, and packs those exact bytes. So the binary
someone gets from npm is provably the binary on the releases page, and
`--provenance` records that it was built by this workflow rather than uploaded
from somebody's laptop.

Seven packages go out. Six carry one binary each and name the machine they are
for through npm's `os` and `cpu` fields, and the seventh is the thin `mcpsnoop`
package that holds the launcher and lists the other six as optional dependencies.
npm installs whichever one matches and skips the rest, which is why there is no
download step at install time and why it works under `--ignore-scripts`.

The order matters and `npmpack -print-order` decides it. The root package
resolves to the other six, so it goes last.

Nothing here can be taken back. **npm does not let a published version be
replaced**, and unpublishing is restricted to a 72 hour window. The packaging
step refuses to write anything unless all six archives are present and match
their checksums, for that reason. If the npm half fails after the GitHub Release
is out, do not re-tag. Re-run the `npm` job on its own from the Actions tab with
`workflow_dispatch`, giving it the same tag.

The first release of a package has to go through the token, because npm's
[trusted publishing](https://docs.npmjs.com/trusted-publishers) is configured in
an existing package's settings. Once all seven exist, each can be switched to
trusted publishing on npmjs.com and the `NPM_TOKEN` secret deleted, which drops
the long-lived credential entirely. That needs npm 11.5.1 or later on the runner.

To see what would be published without publishing it, build the packages from a
downloaded release.

```bash
gh release download vX.Y.Z --dir dist --pattern 'mcpsnoop_*' --pattern 'checksums.txt'
go run ./cmd/npmpack -version vX.Y.Z -dist dist -out dist/npm
```

Pick the version with [SemVer](https://semver.org), and see the full policy in
[CONTRIBUTING](CONTRIBUTING.md#versioning). While on `0.x`, a `0.Y.0` bump may
change behaviour and a `0.y.Z` bump is bug fixes only.
