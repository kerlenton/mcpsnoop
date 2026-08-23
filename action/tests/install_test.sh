#!/usr/bin/env bash
# Exercise action/install.sh against a stand-in for the releases page.
#
# The download is replaced by a curl on PATH that serves a local directory, so
# the checksum verification can be given an archive that does not match and be
# seen to reject it. Without the stand-in a test cannot tamper with anything: the
# real script re-downloads, which quietly restores whatever the test corrupted
# and turns the test into a check that curl works.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
install_sh="$here/../install.sh"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

fail=0
ok() { printf '  ok    %s\n' "$1"; }
bad() {
	printf '  FAIL  %s\n' "$1"
	shift
	for line in "$@"; do printf '        %s\n' "$line"; done
	fail=1
}

# serve builds a fake releases directory and a curl that reads from it.
serve() {
	local dir="$root/serve"
	rm -rf "${dir:?}" "${root:?}/bin"
	mkdir -p "$dir" "$root/bin"
	printf '#!/bin/sh\nexit 0\n' >"$dir/mcpsnoop"
	chmod +x "$dir/mcpsnoop"
	tar -czf "$dir/mcpsnoop_9.9.9_linux_amd64.tar.gz" -C "$dir" mcpsnoop
	sum "$dir/mcpsnoop_9.9.9_linux_amd64.tar.gz" >"$dir/sum"
	printf '%s  mcpsnoop_9.9.9_linux_amd64.tar.gz\n' "$(cat "$dir/sum")" >"$dir/checksums.txt"

	cat >"$root/bin/curl" <<-'CURL'
		#!/usr/bin/env bash
		# Serve the last URL argument out of $SERVE_DIR, into the file after --output.
		out=""; url=""
		while [ $# -gt 0 ]; do
		  case "$1" in
		    --output) out="$2"; shift 2 ;;
		    -*) shift ;;
		    *) url="$1"; shift ;;
		  esac
		done
		src="$SERVE_DIR/$(basename "$url")"
		[ -f "$src" ] || { echo "curl: (56) not found: $url" >&2; exit 22; }
		cp "$src" "$out"
	CURL
	chmod +x "$root/bin/curl"
}

sum() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

run() {
	rm -rf "$root/temp"
	mkdir -p "$root/temp"
	: >"$root/path"
	env PATH="$root/bin:$PATH" SERVE_DIR="$root/serve" \
		RUNNER_TEMP="$root/temp" GITHUB_PATH="$root/path" \
		RUNNER_OS=Linux RUNNER_ARCH=X64 GITHUB_ACTION_REF=v9.9.9 \
		bash "$install_sh" 2>&1
}

echo "install.sh"

serve
if out="$(run)"; then
	if [ -x "$root/temp/mcpsnoop-9.9.9/mcpsnoop" ] && grep -q "mcpsnoop-9.9.9" "$root/path"; then
		ok "installs a release whose archive matches its checksum"
	else
		bad "the install reported success without leaving a binary on PATH" "$out"
	fi
else
	bad "a well formed release was rejected" "$out"
fi

serve
# One byte on the end. The checksums file still describes the original.
printf 'x' >>"$root/serve/mcpsnoop_9.9.9_linux_amd64.tar.gz"
if out="$(run)"; then
	bad "installed an archive that does not match its checksum" "$out"
else
	case "$out" in
	*"does not match its checksum"*) ok "rejects an archive that does not match its checksum" ;;
	*) bad "rejected it for the wrong reason" "$out" ;;
	esac
fi

serve
# A checksums file that vouches for something else entirely.
printf '%s  some-other-artifact.tar.gz\n' "$(cat "$root/serve/sum")" >"$root/serve/checksums.txt"
if out="$(run)"; then
	bad "installed an archive the checksums file never mentions" "$out"
else
	case "$out" in
	*"does not list exactly one entry"*) ok "rejects an archive the checksums file does not vouch for" ;;
	*) bad "rejected it for the wrong reason" "$out" ;;
	esac
fi

serve
# Two lines for one name. Taking either would be a guess.
cat "$root/serve/checksums.txt" "$root/serve/checksums.txt" >"$root/serve/c" && mv "$root/serve/c" "$root/serve/checksums.txt"
if out="$(run)"; then
	bad "installed an archive listed twice with no way to tell which line is authoritative" "$out"
else
	# The message matters, not just the rejection. Two identical lines also fail
	# the byte comparison further down, with "does not match its checksum",
	# which sends the reader after a corrupt download that never happened.
	case "$out" in
	*"does not list exactly one entry"*) ok "rejects a checksums file that lists the archive more than once, and says so" ;;
	*) bad "rejected it, but blamed the wrong thing" "$out" ;;
	esac
fi

serve
# A hasher that escapes its own output, which is what GNU coreutils does when the
# name it was handed contains a backslash: it escapes the line and prefixes the
# whole thing with one. RUNNER_TEMP on a Windows runner is D:\a\_temp, so every
# hash came back as \<hash> and matched nothing, and the install failed on
# releases that were fine.
#
# A stand-in rather than a real path with a backslash in it. The escaping is what
# is under test; putting a backslash in a directory name instead drags in how tar
# and the filesystem treat one, which differs between platforms and is not the
# bug. macOS also ships a sha256sum that does not escape at all, so a test using
# whatever is installed proves nothing on a developer's machine.
# The real hasher resolved to an absolute path here, before the stand-in goes on
# PATH under the same name. Written as a bare name it would find itself, and a
# stand-in that recurses returns a pile of backslashes and nothing to compare, so
# the case would pass whatever the script did.
real_hasher=""
for candidate in sha256sum shasum; do
	if path="$(command -v "$candidate")"; then
		case "$candidate" in
		sha256sum) real_hasher="$path" ;;
		shasum) real_hasher="$path -a 256" ;;
		esac
		break
	fi
done
[ -n "$real_hasher" ] || bad "no hasher to build the stand-in from, so the case below would prove nothing"
cat >"$root/bin/sha256sum" <<GNU
#!/usr/bin/env bash
line="\$($real_hasher "\$@")"
hash="\${line%% *}"
# Named as an argument, escape the way GNU does when the name holds a backslash.
# Read from stdin there is no name to escape, which is the whole way out.
if [ \$# -gt 0 ]; then printf '\\\\%s  %s\\n' "\$hash" "\$1"; else printf '%s  -\\n' "\$hash"; fi
GNU
chmod +x "$root/bin/sha256sum"
# The stand-in has to produce the right hash for the file, or it is testing
# nothing but itself.
[ "$("$root/bin/sha256sum" <"$root/serve/mcpsnoop_9.9.9_linux_amd64.tar.gz" | cut -d' ' -f1)" = "$(cat "$root/serve/sum")" ] ||
	bad "the stand-in hasher does not agree with the real one, so the case below proves nothing"
if out="$(run)"; then
	ok "is not fooled by a hasher that escapes its own output, as GNU does on Windows"
else
	bad "an escaping hasher broke the verification" "$out"
fi
rm -f "$root/bin/sha256sum"

serve
# A checksums file that also vouches for an artifact whose name contains the
# archive's. A release that grows a signature or an SBOM alongside each archive
# produces exactly this, and a substring match would then have two candidates
# and no way to tell which line describes the file it is about to unpack.
{
	printf '%s  %s\n' "$(cat "$root/serve/sum")" "mcpsnoop_9.9.9_linux_amd64.tar.gz.sig"
	cat "$root/serve/checksums.txt"
} >"$root/serve/c" && mv "$root/serve/c" "$root/serve/checksums.txt"
if out="$(run)"; then
	ok "picks its own line out of a checksums file listing a similarly named artifact"
else
	bad "a checksums file listing a sibling artifact broke the install" "$out"
fi

serve
# Each of these must be turned away by the validation, not by the download
# failing later. A ref that reaches curl has already been used to build a URL,
# which is the whole thing the validation exists to prevent, and a 404 would let
# the test pass while it did.
refused=1
for bad_ref in main v1 "0.20.0" "v1.0.0';whoami;'" "v1.0.0/../../other/repo" "v1.0.0
v2"; do
	if out="$(rm -rf "$root/temp" && mkdir -p "$root/temp" && : >"$root/path" && env PATH="$root/bin:$PATH" SERVE_DIR="$root/serve" \
		RUNNER_TEMP="$root/temp" GITHUB_PATH="$root/path" RUNNER_OS=Linux RUNNER_ARCH=X64 \
		GITHUB_ACTION_REF="$bad_ref" bash "$install_sh" 2>&1)"; then
		bad "accepted \"$bad_ref\" as a release tag" "$out"
		refused=0
		continue
	fi
	case "$out" in
	*"is not a release tag"* | *"contains characters a release tag cannot have"*) ;;
	*)
		bad "\"$bad_ref\" failed, but not at the validation" "$out"
		refused=0
		;;
	esac
done
[ "$refused" = 1 ] && ok "refuses a ref that is not a release tag before it reaches a URL"

serve
if out="$(rm -rf "$root/temp" && mkdir -p "$root/temp" && : >"$root/path" && env PATH="$root/bin:$PATH" SERVE_DIR="$root/serve" \
	RUNNER_TEMP="$root/temp" GITHUB_PATH="$root/path" RUNNER_OS=Linux RUNNER_ARCH=RISCV \
	GITHUB_ACTION_REF=v9.9.9 bash "$install_sh" 2>&1)"; then
	bad "claimed to install a binary for a platform no release is built for" "$out"
else
	case "$out" in
	*"no mcpsnoop release is built for"*) ok "names the platform it has nothing for, and says what to do instead" ;;
	*) bad "the message does not name the platform" "$out" ;;
	esac
fi

exit "$fail"
