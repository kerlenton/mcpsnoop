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
# None of this can tell whether the pin is the newest release. That is a person's
# job at release time and RELEASING.md says so. What it can do is insist the two
# agree, and that the page says the pin is the reader's to choose.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readme="$here/../../README.md"

fail=0
ok() { printf '  ok    %s\n' "$1"; }
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

if grep -q "releases page" "$readme"; then
	ok "and points at the releases page, so the pin reads as the reader's choice"
else
	bad "README.md pins a version without pointing at the releases page" \
		"the number then reads as an instruction rather than an example, and it goes stale"
fi

exit "$fail"
