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
checks the runner's npm before the publish, so that cause is ruled out up front.

Which leaves the trusted publisher itself as what a 401 or 404 at the publish
means. npm's OIDC exchange is documented never to throw, so a publisher pinned
to the wrong workflow filename, or scoped to a GitHub environment this job never
enters, or missing `npm publish` under allowed actions, produces silence and then
an unhelpful status from the registry. Check the seven configurations on
npmjs.com before suspecting anything in this repository. The publish keeps
`--provenance` for the same reason: provenance is generated inside the success
branch of that exchange, so asking for it outright turns a silent failure into a
failed publish rather than a published version nobody can verify.

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

## Prereleases

A prerelease is a tag with a semver prerelease part, and it must be spelled
`-rc.N`. Both halves of the release then keep it away from the people who did not
ask for it. npm publishes it under the `next` dist-tag, so `npx mcpsnoop` stays
on the last stable version, and the publish step refuses to run if those two
disagree. GoReleaser flags the GitHub Release a prerelease, and GitHub will not
give the Latest badge to one.

The spelling is not cosmetic. mcpsnoop is in homebrew-core with autobump on and
no `livecheck` block, so the only thing keeping a prerelease out of
`brew install mcpsnoop` is Homebrew's list of unstable keywords, which contains
`rc` but not `next`. A tag spelled `v1.0.0-next.1` would be bumped to every
Homebrew user.

Four more things are true of a prerelease and worth knowing before cutting one.

**It is as permanent as a final release.** npm's 72 hour unpublish window only
applies while nothing in the registry depends on the package, and the root
package lists all six platform packages. The moment the seventh publish lands,
the six each have a public dependent, so a withdrawal has to run in the reverse
of the publish order, root first.

**Do not put the final tag on the rc's commit.** `git:prerelease_suffix` in
`.goreleaser.yaml` makes git sort the final above the rc, so this is handled, but
the failure it prevents is quiet: GoReleaser would take the rc as the current tag
and rebuild it, and the npm job would then look for a release that was never
created.

**The final release's notes will start from the rc, not from the last stable
version**, because `github-native` generates them for the range since the nearest
preceding tag. Set `GORELEASER_PREVIOUS_TAG` in the goreleaser job for that
release, or fix the notes by hand afterwards.

**Retire the `next` dist-tag** once the final version is out, or `npm i
mcpsnoop@next` keeps installing a release candidate nobody supports.

```bash
for p in $(go run ./cmd/npmpack -print-order); do
  npm dist-tag add "$p@X.Y.Z" next
done
```

Do not make the *first* release of a package a prerelease. npm's handling of
`latest` on a package that has never been published is not documented, and the
first release is the one time there is no existing `latest` to protect.

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

## GitHub Marketplace

The repository root carries `action.yml`, so each release can also be published
to the Marketplace. That part is manual and stays manual: GoReleaser creates the
release through the API, and the "Publish this Action to the GitHub Marketplace"
checkbox only appears when a release is edited in the web UI. Publishing also
asks for two-factor authentication.

So after a release lands, open it on the releases page, tick the box, and publish
again.

Skipping it breaks nothing. `uses: kerlenton/mcpsnoop@vX.Y.Z` resolves the
repository at that git ref and never consults the Marketplace, which the action's
own self-test proves on every pull request: it runs the action from the working
tree, with no listing involved. What the box affects is the version the listing
advertises, so forgetting it leaves the page recommending an older release than
the one that exists.

The listing body is this repository's README, read from the default branch rather
than from the release, so a documentation fix reaches the page without
republishing anything. The version beside it comes from the release, so the two
drift: after a tag, the sidebar advertises the new version while the snippet in
the text still pins the old one, and the reader copies the snippet. **Update the
`uses:` versions in README.md before tagging**, which `action/tests/docs_test.sh`
keeps consistent with each other but cannot keep current.

The listing is keyed on the `name` field in `action.yml`, not on the repository,
so that field must not change. Do not rename `action.yml` to `action.yaml`
either, and do not create a floating major tag such as `v1`: Homebrew autobumps
this project by reading raw git tags through `/\D*(.+)/`, which reads `v1` as
version 1, above every 0.x release, and would bump every `brew install mcpsnoop`
user to a tag that moves under them.

Pick the version with [SemVer](https://semver.org), and see the full policy in
[CONTRIBUTING](CONTRIBUTING.md#versioning). While on `0.x`, a `0.Y.0` bump may
change behaviour and a `0.y.Z` bump is bug fixes only.
