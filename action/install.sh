#!/usr/bin/env bash
# Put the mcpsnoop binary of one release on PATH, having proved it is the binary
# that release published.
#
# This runs in other people's repositories, so it fails loudly and never guesses.
# Every input that reaches a URL, a filesystem path or another interpreter is
# validated first, and the archive is checked against the checksums file
# published beside it before anything is unpacked or run.
set -euo pipefail

die() {
	printf 'mcpsnoop-action: %s\n' "$1" >&2
	shift
	for line in "$@"; do printf '  %s\n' "$line" >&2; done
	exit 1
}

# ---------------------------------------------------------------- the version

version="${MCPSNOOP_VERSION:-}"
if [ -z "$version" ]; then
	# The ref the caller pinned. `uses: kerlenton/mcpsnoop@v0.21.0` asks for the
	# action from that release, so it asks for that release's binary too, and
	# there is no default in this file to go stale.
	version="${GITHUB_ACTION_REF:-}"
fi
case "$version" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*)
	die "cannot tell which mcpsnoop to install." \
		"" \
		"The version comes from the ref you pinned the action to, and \"${version:-<empty>}\" is not a release tag." \
		"Pin a release:" \
		"" \
		"  - uses: kerlenton/mcpsnoop@v0.21.0" \
		"" \
		"or, when pinning a branch or a commit, say which binary to use:" \
		"" \
		"  - uses: kerlenton/mcpsnoop@<sha>" \
		"    with:" \
		"      version: v0.21.0" \
		"" \
		"See https://github.com/kerlenton/mcpsnoop/releases"
	;;
esac
# Belt and braces. The check above already excludes everything below, and this
# one says so out loud, because the value goes into a URL and, on Windows, into
# another interpreter's command line.
case "$version" in
*[!0-9A-Za-z.v+-]*) die "version \"$version\" contains characters a release tag cannot have" ;;
esac
plain="${version#v}"

# --------------------------------------------------------------- the platform

case "${RUNNER_OS:-}/${RUNNER_ARCH:-}" in
Linux/X64) goos=linux goarch=amd64 ext=tar.gz binary=mcpsnoop ;;
Linux/ARM64) goos=linux goarch=arm64 ext=tar.gz binary=mcpsnoop ;;
macOS/X64) goos=darwin goarch=amd64 ext=tar.gz binary=mcpsnoop ;;
macOS/ARM64) goos=darwin goarch=arm64 ext=tar.gz binary=mcpsnoop ;;
Windows/X64) goos=windows goarch=amd64 ext=zip binary=mcpsnoop.exe ;;
Windows/ARM64) goos=windows goarch=arm64 ext=zip binary=mcpsnoop.exe ;;
*)
	die "no mcpsnoop release is built for ${RUNNER_OS:-<unknown OS>} on ${RUNNER_ARCH:-<unknown arch>}." \
		"" \
		"Releases cover Linux, macOS and Windows on X64 and ARM64." \
		"On anything else, install it yourself and set install: false:" \
		"" \
		"  go install github.com/kerlenton/mcpsnoop/cmd/mcpsnoop@$version"
	;;
esac

archive="mcpsnoop_${plain}_${goos}_${goarch}.${ext}"
base="https://github.com/kerlenton/mcpsnoop/releases/download/${version}"
dir="${RUNNER_TEMP:?RUNNER_TEMP is not set, which means this is not running on a GitHub runner}/mcpsnoop-${plain}"

if [ -x "$dir/$binary" ]; then
	# A workflow that calls the action twice should not download twice.
	printf 'mcpsnoop %s is already installed at %s\n' "$plain" "$dir"
else
	mkdir -p "$dir"
	fetch() {
		curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
			--output "$2" "$1" ||
			die "could not download $1" "" "Check that $version is a published release." \
				"See https://github.com/kerlenton/mcpsnoop/releases"
	}
	fetch "$base/$archive" "$dir/$archive"
	fetch "$base/checksums.txt" "$dir/checksums.txt"

	# Exactly one line, matched on the whole name rather than by a substring, so
	# a checksums file that ever grows a similarly named artifact cannot make
	# this silently verify the wrong one.
	want="$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1; n++ } END { if (n != 1) exit 1 }' "$dir/checksums.txt")" ||
		die "checksums.txt for $version does not list exactly one entry for $archive"

	# The file on stdin rather than named as an argument. Given a name, GNU
	# coreutils escapes the line it prints when that name contains a backslash,
	# and prefixes the whole thing with one. RUNNER_TEMP on a Windows runner is
	# D:\a\_temp, so every hash came back as \<hash> and never matched anything.
	if command -v sha256sum >/dev/null 2>&1; then
		got="$(sha256sum <"$dir/$archive" | cut -d' ' -f1)"
	elif command -v shasum >/dev/null 2>&1; then
		got="$(shasum -a 256 <"$dir/$archive" | cut -d' ' -f1)"
	else
		die "neither sha256sum nor shasum is available, so the download cannot be verified"
	fi
	if [ "$got" != "$want" ]; then
		die "$archive does not match its checksum, so it is not the archive $version published." \
			"  expected $want" \
			"  got      $got"
	fi

	case "$ext" in
	tar.gz) tar -xzf "$dir/$archive" -C "$dir" "$binary" ;;
	zip)
		if command -v unzip >/dev/null 2>&1; then
			unzip -q -o "$dir/$archive" "$binary" -d "$dir"
		else
			# No caller-controlled text reaches this command line. The version is
			# validated above and everything else comes from RUNNER_TEMP.
			powershell.exe -NoProfile -NonInteractive -Command \
				"Expand-Archive -LiteralPath '$(cygpath -w "$dir/$archive")' -DestinationPath '$(cygpath -w "$dir")' -Force" ||
				die "could not unpack $archive"
		fi
		;;
	esac
	chmod +x "$dir/$binary" 2>/dev/null || true
	[ -s "$dir/$binary" ] || die "$archive unpacked without a $binary in it"
fi

printf '%s\n' "$dir" >>"$GITHUB_PATH"

# The resolved version belongs in every job log, so a report that looks wrong can
# be traced to the build that produced it without re-running anything.
"$dir/$binary" --version
