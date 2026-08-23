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
`npx mcpsnoop -- node build/index.js` works without a Go toolchain. It needs no
secret either. Each of the seven packages names this workflow in this repository
as its [trusted publisher](https://docs.npmjs.com/trusted-publishers), so the
runner mints a short-lived OIDC token for the publish and npm attaches the
provenance attestation on its own. There is no long-lived npm credential to
leak, rotate, or let expire.

That is why the job runs node 24 rather than the 22 that CI runs. Authenticating
by OIDC needs npm 11.5.1 or later, and node 22 still carries npm 10. A step
checks this before the publish, because an npm too old fails with a 401 that
reads like a credential problem and is not one.

The job downloads the archives the release just published, checks each one
against the `checksums.txt` published beside it, and packs those exact bytes. So
the binary someone gets from npm is provably the binary on the releases page,
and the attestation records that it was built by this workflow rather than
uploaded from somebody's laptop.

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
`workflow_dispatch`, giving it the same tag. It skips whatever is already on the
registry, so a publish that died partway through picks up where it stopped.

A prerelease goes out under the `next` tag rather than claiming `latest`, so a
`vX.Y.Z-rc.1` does not reach everyone running `npx mcpsnoop`. Do not make the
*first* release of a package a prerelease, though. npm's handling of `latest` on
a package that has never been published is not documented, and the first release
is the one time there is no existing `latest` to protect.

Adding a platform, or moving the workflow to another filename, means visiting
all seven packages under Settings and Trusted publishing on npmjs.com. The
trusted publisher is pinned to `release.yml` in `kerlenton/mcpsnoop` by name, so
a rename that looks harmless in the repository stops every publish. A new
package also has to be given its trusted publisher, and that is only possible
once it exists, so the very first publish of one needs a granular token with
publish rights, deleted again straight after.

To see what would be published without publishing it, build the packages from a
downloaded release.

```bash
gh release download vX.Y.Z --dir dist --pattern 'mcpsnoop_*' --pattern 'checksums.txt'
go run ./cmd/npmpack -version vX.Y.Z -dist dist -out dist/npm
```

Pick the version with [SemVer](https://semver.org), and see the full policy in
[CONTRIBUTING](CONTRIBUTING.md#versioning). While on `0.x`, a `0.Y.0` bump may
change behaviour and a `0.y.Z` bump is bug fixes only.
