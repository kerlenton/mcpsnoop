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
	out upload false
	printf '### mcpsnoop\n\n**Could not check.** See the step log; this is not a finding.\n' \
		>>"${GITHUB_STEP_SUMMARY:-/dev/null}"
	exit 0
}

session="${MCPSNOOP_SESSION:-}"
report="${RUNNER_TEMP:-}/mcpsnoop.sarif"
[ -n "$session" ] && [ -n "${RUNNER_TEMP:-}" ] || {
	# Neither is anything a caller can get wrong through the action's inputs, so
	# there is no help to offer beyond saying which one is missing.
	printf 'mcpsnoop-action: %s is not set\n' "$([ -z "$session" ] && echo "the session input" || echo RUNNER_TEMP)" >&2
	exit 1
}

# The action reports through a file, so a capture piped in on stdin has nothing
# for a result to point at and every alert would arrive with no location.
[ "$session" = "-" ] && refuse "session cannot be -." \
	"" \
	"The report points at the file each finding came from, and a capture read from" \
	"stdin has no file. Write it out first and pass the path."
[ -e "$session" ] || refuse "there is no capture at \"$session\"." \
	"" \
	"session is a path to a .jsonl capture, relative to the repository root." \
	"Record one by wrapping your server with mcpsnoop before this step."

flags=()
if [ -n "${MCPSNOOP_FAIL_ON:-}" ]; then
	flags+=(--fail-on "$MCPSNOOP_FAIL_ON")
fi
# xargs rather than word splitting, so a value with a space in it survives being
# quoted the way it would be on a command line.
#
# Its exit status has to be caught here, before the tokens are read. xargs emits
# whatever it parsed before giving up, so an unbalanced quote yields a shorter
# command line and a zero status, and `--expect-tool 'oops` quietly becomes
# `--expect-tool` swallowing whatever came next. Reading it through a pipe or a
# process substitution hides that failure completely.
if [ -n "${MCPSNOOP_ARGS:-}" ]; then
	if ! split="$(printf '%s' "$MCPSNOOP_ARGS" | xargs -n1 printf '%s\n' 2>&1)"; then
		refuse "args is not something a command line can be read out of." \
			"" \
			"  ${split##*: }" \
			"" \
			"Quote args the way you would on a command line:" \
			"" \
			"  args: --expect-tool search --max-server-duration 500ms"
	fi
	while IFS= read -r token; do
		[ -n "$token" ] || continue
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
	done <<<"$split"
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
	# The backticks below are markdown for the summary, not command substitution.
	# shellcheck disable=SC2016
	case "$outcome" in
	passed) printf '**Passed.** `%s` violated nothing that was gated on.\n' "$session" ;;
	findings) printf '**Findings.** `%s` violated a gated signal.\n' "$session" ;;
	error) printf '**Could not check `%s`.** See the step log; this is not a finding.\n' "$session" ;;
	esac
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
