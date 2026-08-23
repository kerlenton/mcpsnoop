#!/usr/bin/env bash
# Exercise action/check.sh and action/gate.sh over every verdict they can reach.
#
# What matters here is not what mcpsnoop finds, which its own tests cover, but
# what the action does with each answer: whether it uploads, and whether the job
# ends up red. Getting that wrong in either direction is worse than a wrong
# finding. Green on a run that checked nothing is the failure this whole shape
# exists to prevent, and red on a real finding keeps the report out of the place
# it exists to reach.
#
# Captures are written here rather than committed, because *.jsonl is ignored in
# this repository and a fixture git silently refused to track would make every
# case below pass against nothing.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check_sh="$here/../check.sh"
gate_sh="$here/../gate.sh"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

command -v mcpsnoop >/dev/null 2>&1 || {
	echo "check.sh: mcpsnoop is not on PATH; build it first (go build -o <dir>/mcpsnoop ./cmd/mcpsnoop)" >&2
	exit 1
}

fail=0
ok() { printf '  ok    %s\n' "$1"; }
bad() {
	printf '  FAIL  %s\n' "$1"
	shift
	for line in "$@"; do printf '        %s\n' "$line"; done
	fail=1
}

envelope() { # seq direction json
	printf '{"session_id":"t","server_label":"t","seq":%s,"ts":"2026-01-01T00:00:00Z","direction":"%s","transport":"stdio","raw":%s}\n' "$1" "$2" "$3"
}
{
	envelope 1 c2s '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ok"}}'
	envelope 2 s2c '{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fine"}]}}'
} >"$root/clean.jsonl"
{
	envelope 1 c2s '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}'
	envelope 2 s2c '{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such tool"}}'
} >"$root/dirty.jsonl"
printf 'this is not a capture\n' >"$root/garbage.jsonl"

# run drives check.sh and then gate.sh the way action.yml wires them, and prints
# what a job would have ended up doing.
run() { # session [env=value ...]
	local session="$1"
	shift
	rm -rf "${root:?}/temp" "${root:?}/home"
	mkdir -p "$root/temp"
	: >"$root/out"
	: >"$root/summary"
	env RUNNER_TEMP="$root/temp" MCPSNOOP_HOME="$root/home" \
		GITHUB_OUTPUT="$root/out" GITHUB_STEP_SUMMARY="$root/summary" \
		MCPSNOOP_SESSION="$session" "$@" bash "$check_sh" >"$root/log" 2>&1
	check_code=$?
	outcome="$(sed -n 's/^outcome=//p' "$root/out")"
	upload="$(sed -n 's/^upload=//p' "$root/out")"
	sarif="$(sed -n 's/^sarif=//p' "$root/out")"
	env MCPSNOOP_OUTCOME="$outcome" MCPSNOOP_SESSION="$session" "$@" \
		bash "$gate_sh" >>"$root/log" 2>&1
	gate_code=$?
}

expect() { # label wantOutcome wantUpload wantGate
	local label="$1" wo="$2" wu="$3" wg="$4"
	local got="outcome=$outcome upload=$upload gate=$gate_code"
	local want="outcome=$wo upload=$wu gate=$wg"
	if [ "$check_code" != 0 ]; then
		bad "$label" "check.sh exited $check_code; it must never fail, or the steps after it have no verdict to read" "$(cat "$root/log")"
	elif [ "$got" != "$want" ]; then
		bad "$label" "got  $got" "want $want" "$(cat "$root/log")"
	else
		ok "$label"
	fi
}

echo "check.sh"

run "$root/clean.jsonl"
expect "a clean capture uploads and passes" passed true 0

run "$root/dirty.jsonl"
expect "a finding uploads and fails" findings true 1
if [ -s "$sarif" ] && grep -q '"mcpsnoop"' "$sarif"; then
	ok "and leaves a SARIF report behind"
else
	bad "a finding left no usable report to upload" "$(cat "$root/log")"
fi

run "$root/dirty.jsonl" MCPSNOOP_FAIL_ON_FINDINGS=false
expect "a finding can be reported without failing" findings true 0

run "$root/dirty.jsonl" MCPSNOOP_FAIL_ON=pending
expect "a finding nobody gated on passes" passed true 0

run "$root/clean.jsonl" MCPSNOOP_UPLOAD=false
expect "upload can be turned off without changing the verdict" passed false 0

# Everything below is the same thing said six ways: the check did not happen.
# None of it may pass, and none of it may be suppressed by fail-on-findings,
# because a run that could not look has not approved anything.
for case in \
	"a path that is not there|$root/no-such-file.jsonl|" \
	"a file that is not a capture|$root/garbage.jsonl|" \
	"a capture piped in on stdin|-|" \
	"args changing the format|$root/clean.jsonl|MCPSNOOP_ARGS=--format text" \
	"args asking for help text|$root/clean.jsonl|MCPSNOOP_ARGS=--help" \
	"args with an unbalanced quote|$root/clean.jsonl|MCPSNOOP_ARGS=--expect-tool 'oops"; do
	IFS='|' read -r label session extra <<<"$case"
	if [ -n "$extra" ]; then run "$session" "$extra"; else run "$session"; fi
	expect "$label is an error, not a finding" error false 1

	if [ -n "$extra" ]; then run "$session" "$extra" MCPSNOOP_FAIL_ON_FINDINGS=false; else run "$session" MCPSNOOP_FAIL_ON_FINDINGS=false; fi
	[ "$gate_code" = 1 ] || bad "$label was suppressed by fail-on-findings" "gate exited $gate_code"

	[ -z "$sarif" ] || [ ! -e "$sarif" ] ||
		bad "$label left a file where the report goes, which the upload would take for one"
done

# The two safeguards, pinned one at a time.
#
# The exit code and the shape of the report each decide the verdict on their own,
# and against the real CLI they always agree, so either one alone keeps every
# case above green and neither is actually being tested. A stand-in that
# disagrees with itself separates them. It also says what happens if the CLI ever
# regresses, which is the whole reason for having both.
stub() { # exitCode stdout
	mkdir -p "$root/stub"
	{
		printf '#!/usr/bin/env bash\n'
		printf 'printf %%s %s\n' "$(printf '%q' "$2")"
		printf 'exit %s\n' "$1"
	} >"$root/stub/mcpsnoop"
	chmod +x "$root/stub/mcpsnoop"
}
# $schema is a JSON key, not a shell variable, so the quoting is deliberate.
# shellcheck disable=SC2016
valid_sarif='{"$schema":"x","version":"2.1.0","runs":[{"tool":{"driver":{"name":"mcpsnoop"}},"results":[]}]}'

stub 2 "$valid_sarif"
PATH="$root/stub:$PATH" run "$root/clean.jsonl"
expect "a report that looks right does not rescue a run that failed" error false 1

stub 0 "Check a captured session against signals (errors, invalid frames, ...)"
PATH="$root/stub:$PATH" run "$root/clean.jsonl"
expect "a clean exit does not make help text a report" error false 1

stub 1 "session t: errors=1 ... check failed: error"
PATH="$root/stub:$PATH" run "$root/clean.jsonl"
expect "nor does a finding make a text summary one" error false 1

# For these two the message is the whole value of the refusal. Both end in an
# error anyway if they are let through, --format because the report then is not
# one and - because there is no such file, so the outcome cannot tell whether the
# refusal is there and the caller is left to work out what went wrong.
run "$root/clean.jsonl" "MCPSNOOP_ARGS=--format text"
grep -q "args cannot set --format" "$root/log" ||
	bad "refusing --format did not say that is what happened" "$(cat "$root/log")"

run "-"
grep -q "session cannot be -" "$root/log" ||
	bad "refusing stdin did not say why a report needs a file" "$(cat "$root/log")"

# install: false with nothing installed. Left alone this is a bare "command not
# found" from the shell, which reads as a broken action rather than as the one
# mistake it almost always is.
# Every directory holding a mcpsnoop taken out of PATH, rather than PATH
# replaced, since the harness itself needs the ordinary tools to run at all.
# Every one, not the first: a machine can carry an installed copy as well as the
# build this suite put there, and leaving either behind proves nothing.
without_mcpsnoop="$PATH"
for _ in 1 2 3 4 5; do
	found="$(PATH="$without_mcpsnoop" bash -c 'command -v mcpsnoop' 2>/dev/null)" || break
	[ -n "$found" ] || break
	without_mcpsnoop="$(printf '%s' "$without_mcpsnoop" | tr ':' '\n' | grep -vFx "$(dirname "$found")" | paste -sd: -)"
done
if PATH="$without_mcpsnoop" bash -c 'command -v mcpsnoop' >/dev/null 2>&1; then
	bad "could not build a PATH without mcpsnoop on it, so the case below would prove nothing"
else
	PATH="$without_mcpsnoop" run "$root/clean.jsonl"
	expect "a missing binary is an error, not a finding" error false 1
	grep -q "GITHUB_PATH" "$root/log" ||
		bad "the missing-binary message does not say how to put it on PATH" "$(cat "$root/log")"
fi

# A verdict that was never written is the same as an error: a check step that
# died before writing one has approved nothing.
if env MCPSNOOP_SESSION=x bash "$gate_sh" >/dev/null 2>&1; then
	bad "an unset verdict did not fail"
else
	ok "an unset verdict fails rather than crashing the gate"
fi

exit "$fail"
