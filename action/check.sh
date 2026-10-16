#!/usr/bin/env bash
# Run `mcpsnoop check` over one capture and record the verdict, without ever
# failing.
#
# Not failing is the point. The report has to reach the Security tab on exactly
# the runs that have something to report, and a step that failed here would stop
# the upload that follows. The verdict goes to an output instead, and a later
# step turns it into the job's result.
set -uo pipefail

out() { printf '%s=%s\n' "$1" "$2" >>"${GITHUB_OUTPUT:-/dev/null}"; }

# refuse says why, records that nothing was checked, and returns cleanly.
#
# Cleanly, because this step must not fail. Failing it would leave the steps
# after it with no verdict to read, and the one that turns a verdict into the
# job's result is the only place that decides whether the job is red. A run that
# never got as far as looking is an error there, and an error there is not
# suppressible, so nothing is being let through by returning zero here.
refuse() {
	printf 'mcpsnoop-action: %s\n' "$1" >&2
	shift
	for line in "$@"; do printf '  %s\n' "$line" >&2; done
	out outcome error
	out exit-code ""
	out sarif ""
	out capture ""
	out upload false
	printf '### mcpsnoop\n\n**Could not check.** See the step log; this is not a finding.\n' \
		>>"${GITHUB_STEP_SUMMARY:-/dev/null}"
	exit 0
}

session="${MCPSNOOP_SESSION:-}"
recording="${MCPSNOOP_REPLAY:-}"
report="${RUNNER_TEMP:-}/mcpsnoop.sarif"
# Not anything a caller can get wrong through the action's inputs, so there is
# no help to offer beyond saying what is missing.
if [ -z "${RUNNER_TEMP:-}" ]; then
	printf 'mcpsnoop-action: RUNNER_TEMP is not set, which means this is not running on a GitHub runner\n' >&2
	exit 1
fi

if [ -n "$session" ] && [ -n "$recording" ]; then
	refuse "session and replay are both set, and the action checks one capture." \
		"" \
		"session checks a capture you recorded in an earlier step. replay records" \
		"one by replaying a committed recording against server. Keep the one you mean."
fi
if [ -z "$session" ] && [ -z "$recording" ]; then
	refuse "neither session nor replay is set, so there is nothing to check." \
		"" \
		"  session: artifacts/session.jsonl       a capture an earlier step recorded" \
		"" \
		"or" \
		"" \
		"  replay: tests/mcp/session.jsonl        a committed recording to replay" \
		"  server: node dist/index.js             against the server this job built"
fi

# words splits a value into the global array words_out the way a command line
# would read it, so a value with a space in it survives being quoted.
#
# xargs rather than word splitting, and its exit status has to be caught here,
# before the tokens are read. xargs emits whatever it parsed before giving up, so
# an unbalanced quote yields a shorter command line and a zero status, and
# `--expect-tool 'oops` quietly becomes `--expect-tool` swallowing whatever came
# next. Reading it through a pipe or a process substitution hides that failure
# completely.
words() { # input value example
	words_out=()
	local split token
	if ! split="$(printf '%s' "$2" | xargs -n1 printf '%s\n' 2>&1)"; then
		refuse "$1 is not something a command line can be read out of." \
			"" \
			"  ${split##*: }" \
			"" \
			"Quote $1 the way you would on a command line:" \
			"" \
			"  $1: $3"
	fi
	while IFS= read -r token; do
		[ -n "$token" ] && words_out+=("$token")
	done <<<"$split"
}

# The action reports through a file, so a capture piped in on stdin has nothing
# for a result to point at and every alert would arrive with no location.
[ "$session" = "-" ] && refuse "session cannot be -." \
	"" \
	"The report points at the file each finding came from, and a capture read from" \
	"stdin has no file. Write it out first and pass the path."
if [ -n "$session" ]; then
	[ -e "$session" ] || refuse "there is no capture at \"$session\"." \
		"" \
		"session is a path to a .jsonl capture, relative to the repository root." \
		"Record one by wrapping your server with mcpsnoop before this step."
else
	[ -e "$recording" ] || refuse "there is no recording at \"$recording\"." \
		"" \
		"replay is a path to a committed .jsonl capture, relative to the repository" \
		"root. Record one once by wrapping your server with mcpsnoop in your own" \
		"client, and commit the session log it writes."
	[ -n "${MCPSNOOP_SERVER:-}" ] || [ -n "${MCPSNOOP_TARGET:-}" ] ||
		refuse "replay needs server, the command that starts the server under test, or target, the URL of one already running." \
			"" \
			"  server: node dist/index.js                    a stdio server" \
			"" \
			"  target: http://localhost:3000/mcp             an HTTP server, started by" \
			"  server: node dist/http.js                     server or already running"
fi

command -v mcpsnoop >/dev/null 2>&1 || refuse "mcpsnoop is not on PATH." \
	"" \
	"With install: false the binary has to be there already, and putting it on" \
	"PATH in an earlier step needs GITHUB_PATH rather than an export, which" \
	"lasts only as long as the step that ran it:" \
	"" \
	"  echo \"\$RUNNER_TEMP/bin\" >> \"\$GITHUB_PATH\"" \
	"" \
	"Or drop install: false and let the action fetch the release you pinned."

flags=()
if [ -n "${MCPSNOOP_FAIL_ON:-}" ]; then
	flags+=(--fail-on "$MCPSNOOP_FAIL_ON")
fi
if [ -n "${MCPSNOOP_ARGS:-}" ]; then
	words args "$MCPSNOOP_ARGS" "--expect-tool search --max-server-duration 500ms"
	for token in ${words_out[@]+"${words_out[@]}"}; do
		case "$token" in
		--format | --format=*)
			refuse "args cannot set --format." \
				"" \
				"The action reads the SARIF report this step produces, so the format is" \
				"not yours to change. Everything else check accepts is fine here."
			;;
		-h | --help)
			refuse "args cannot ask for help text, which check writes where the report goes."
			;;
		esac
		flags+=("$token")
	done
fi

# A replay produces the capture this step then checks, and its own verdict, a
# regression against the recording, joins the check's.
regressed=false
if [ -n "$recording" ]; then
	server=()
	if [ -n "${MCPSNOOP_SERVER:-}" ]; then
		words server "$MCPSNOOP_SERVER" "node dist/index.js"
		[ "${#words_out[@]}" -gt 0 ] || refuse "server is empty, so there is no server to replay against." \
			"" \
			"  server: node dist/index.js"
		server=("${words_out[@]}")
	fi
	replay_flags=()
	if [ -n "${MCPSNOOP_TARGET:-}" ]; then
		replay_flags+=(--target "$MCPSNOOP_TARGET")
	fi
	if [ -n "${MCPSNOOP_REPLAY_ARGS:-}" ]; then
		words replay-args "$MCPSNOOP_REPLAY_ARGS" "--read-only --wait 30s"
		for token in ${words_out[@]+"${words_out[@]}"}; do
			case "$token" in
			--out | --out=*)
				refuse "replay-args cannot set --out." \
					"" \
					"The action writes the replayed capture where it can check it, and" \
					"hands the path back as the capture output."
				;;
			--exit-code | --exit-code=*)
				continue # the action always asks for it
				;;
			-h | --help)
				refuse "replay-args cannot ask for help text."
				;;
			esac
			replay_flags+=("$token")
		done
	fi
	session="$RUNNER_TEMP/mcpsnoop-replay.jsonl"
	replay_log="$RUNNER_TEMP/mcpsnoop-replay.txt"
	# The command line goes to the log with every --header value hidden, since
	# that is where a credential rides and a log outlives the job.
	shown_flags=()
	hide_next=false
	for token in ${replay_flags[@]+"${replay_flags[@]}"}; do
		if [ "$hide_next" = true ]; then
			shown_flags+=("${token%%:*}: ***")
			hide_next=false
			continue
		fi
		case "$token" in
		--header) hide_next=true ;;
		--header=*)
			value="${token#--header=}"
			shown_flags+=("--header=${value%%:*}: ***")
			continue
			;;
		esac
		shown_flags+=("$token")
	done
	if [ "${#server[@]}" -gt 0 ]; then
		printf 'mcpsnoop replay %s --out %s --exit-code %s -- %s\n' \
			"$recording" "$session" "${shown_flags[*]-}" "${server[*]}"
		mcpsnoop replay "$recording" --out "$session" --exit-code ${replay_flags[@]+"${replay_flags[@]}"} -- "${server[@]}" >"$replay_log"
	else
		printf 'mcpsnoop replay %s --out %s --exit-code %s\n' "$recording" "$session" "${shown_flags[*]-}"
		mcpsnoop replay "$recording" --out "$session" --exit-code ${replay_flags[@]+"${replay_flags[@]}"} >"$replay_log"
	fi
	replay_code=$?
	cat "$replay_log"
	case "$replay_code" in
	0) ;;
	1) regressed=true ;;
	*)
		refuse "the replay of \"$recording\" could not run, so there is no capture to check." \
			"" \
			"Its own message is above. The usual causes are a server command that" \
			"does not start from the repository root, and a recording that is not a" \
			"capture."
		;;
	esac
fi

printf 'mcpsnoop check --format sarif %s -- %s\n' "${flags[*]-}" "$session"
mcpsnoop check --format sarif ${flags[@]+"${flags[@]}"} -- "$session" >"$report"
code=$?

# 1 means the check ran and something failed the gate, so the report is real and
# worth publishing. Anything else non-zero means the check never happened: a
# capture that would not load, a flag that would not parse. The difference is a
# contract the CLI keeps and a test in that repository pins, because without it a
# typo in the session path would read here as a clean run.
case "$code" in
0) outcome=passed ;;
1) outcome=findings ;;
*) outcome=error ;;
esac
# A regression fails the run however clean the replayed capture checks.
if [ "$outcome" = passed ] && [ "$regressed" = true ]; then
	outcome=findings
fi

# Belt and braces against the contract changing under us. A report that is not a
# mcpsnoop SARIF document is not something to hand to code scanning, whatever the
# exit code said. Checked without a JSON parser, since a runner is not obliged to
# carry one.
if [ "$outcome" != error ]; then
	if [ ! -s "$report" ] || ! grep -q '"mcpsnoop"' "$report" 2>/dev/null; then
		printf 'mcpsnoop-action: check exited %s but wrote no usable SARIF report\n' "$code" >&2
		outcome=error
	fi
fi
if [ "$outcome" = error ]; then
	# Nothing here is a report. Leaving the file behind invites the upload step,
	# or a later artifact upload, to treat it as one.
	rm -f "$report"
fi

out outcome "$outcome"
out exit-code "$code"
out sarif "$report"
if [ "$outcome" = error ]; then
	out capture ""
else
	out capture "$(cd "$(dirname "$session")" && pwd)/$(basename "$session")"
fi
# Written as if/else rather than && ||, because in the latter the right-hand
# side also runs when the left-hand side merely fails, and the two branches here
# say opposite things about whether a report should be published.
if [ "$outcome" = error ]; then
	out upload false
else
	out upload "${MCPSNOOP_UPLOAD:-true}"
fi

# The job summary is a convenience and never a reason to fail. python3 is on
# every GitHub-hosted image; a container without one still gets the verdict.
{
	printf '### mcpsnoop\n\n'
	shown="$session"
	[ -n "$recording" ] && shown="$recording"
	# The backticks below are markdown for the summary, not command substitution.
	# shellcheck disable=SC2016
	case "$outcome" in
	passed) printf '**Passed.** `%s` violated nothing that was gated on.\n' "$shown" ;;
	findings)
		if [ "$regressed" = true ]; then
			printf '**Findings.** Replaying `%s` against this build regressed.\n' "$shown"
		else
			printf '**Findings.** `%s` violated a gated signal.\n' "$shown"
		fi
		;;
	error) printf '**Could not check `%s`.** See the step log; this is not a finding.\n' "$shown" ;;
	esac
	if [ -n "$recording" ] && [ -s "${replay_log:-}" ]; then
		printf '\n<details><summary>Replay</summary>\n\n```text\n'
		cat "$replay_log"
		printf '```\n\n</details>\n'
	fi
	if [ "$outcome" != error ] && command -v python3 >/dev/null 2>&1; then
		python3 - "$report" <<-'PY' 2>/dev/null || true
			import collections, json, sys
			results = json.load(open(sys.argv[1]))["runs"][0]["results"]
			if not results:
			    raise SystemExit
			by = collections.Counter((r["level"], r["ruleId"]) for r in results)
			print("\n| Level | Rule | Count |\n|---|---|---|")
			for level in ("error", "warning", "note"):
			    for (lv, rule), n in sorted(by.items()):
			        if lv == level:
			            print(f"| {lv} | `{rule}` | {n} |")
		PY
	fi
} >>"${GITHUB_STEP_SUMMARY:-/dev/null}"

exit 0
