#!/usr/bin/env bash
# Check what the Marketplace listing tells a reader to copy.
#
# The listing's body is this repository's README, read from the default branch
# rather than from a release, so what the page shows is whatever was merged last.
# The README carries the `uses:` line twice, once near the top for someone who
# arrived from the listing and once in the section explaining the inputs. Bumping
# one and forgetting the other leaves the page telling two stories about which
# release to pin, and the reader takes whichever they scroll to first.
#
# It also insists the pin is not older than the newest release tag. Bumping it is
# a step in RELEASING.md, and v0.22.0 and v0.23.0 both shipped with that step
# skipped, so the listing named one release in its sidebar and installed an older
# one from its snippet for a month. The pin may run ahead of the newest tag, which
# is the minute between the release-prep commit and the tag, and never behind.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readme="$here/../../README.md"

fail=0
ok() { printf '  ok    %s\n' "$1"; }
skip() { printf '  skip  %s\n' "$1"; }
bad() {
	printf '  FAIL  %s\n' "$1"
	shift
	for line in "$@"; do printf '        %s\n' "$line"; done
	fail=1
}

echo "docs"

pins="$(grep -oE 'kerlenton/mcpsnoop@v[0-9][^ "`]*' "$readme" | sed 's/.*@//')"
count="$(printf '%s\n' "$pins" | grep -c .)"
distinct="$(printf '%s\n' "$pins" | sort -u | grep -c .)"

if [ "$count" -lt 2 ]; then
	bad "README.md carries $count pinned uses: lines" \
		"it is meant to carry the quickstart one and the one beside the inputs"
elif [ "$distinct" -ne 1 ]; then
	bad "README.md pins more than one version of the action" \
		"$(printf '%s\n' "$pins" | sort -u | tr '\n' ' ')" \
		"the listing would tell two stories about which release to use"
else
	ok "every uses: line in the README pins the same release"
fi

first="$(printf '%s\n' "$pins" | head -1)"
# A full tag, not a branch and not a bare major. There is no floating v1:
# Homebrew autobumps this project by reading raw git tags through /\D*(.+)/,
# which reads v1 as version 1, above every 0.x release.
if printf '%s' "$first" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	ok "and pins a full release tag rather than a branch or a floating major"
else
	bad "README.md pins the action at \"$first\", which is not a full release tag"
fi

# A checkout without tags skips rather than fails, since a missing tag list says
# nothing about the pin. CI fetches them for exactly this.
newest="$(git -C "$here/../.." tag --list 'v*' 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)"
if [ -z "$newest" ]; then
	skip "the pin is not compared with the newest release, since this checkout has no release tags"
elif [ "$(printf '%s\n%s\n' "$first" "$newest" | sort -V | tail -1)" = "$first" ]; then
	ok "and is not older than the newest release, $newest"
else
	bad "README.md hands out $first while $newest is released" \
		"the listing's sidebar names $newest and its snippet installs $first" \
		"run make release-prep TAG=$newest"
fi

if grep -q "releases page" "$readme"; then
	ok "and points at the releases page, so the pin reads as the reader's choice"
else
	bad "README.md pins a version without pointing at the releases page" \
		"the number then reads as an instruction rather than an example, and it goes stale"
fi

exit "$fail"
