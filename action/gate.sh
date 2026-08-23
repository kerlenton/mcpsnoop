#!/usr/bin/env bash
# Turn the verdict the check step recorded into the job's result.
#
# Separate from the check itself so that the report reaches the Security tab
# first. A step that failed before the upload would keep every finding out of the
# place the findings exist to reach.
set -euo pipefail

# Anything other than the two known-good verdicts is an error, an unset value
# included. A step that did not run, or one that died before writing its verdict,
# has not approved anything.
outcome="${MCPSNOOP_OUTCOME:-}"
session="${MCPSNOOP_SESSION:-the capture}"

case "$outcome" in
passed)
	printf 'mcpsnoop: %s passed\n' "$session"
	;;
findings)
	# Suppressible, because a repository may want the alerts filed and the build
	# left alone, which is what code scanning's own required check is for.
	if [ "${MCPSNOOP_FAIL_ON_FINDINGS:-true}" = "false" ]; then
		printf '::notice title=mcpsnoop::%s violated a gated signal. Reported, not failed, because fail-on-findings is false.\n' "$session"
	else
		printf '::error title=mcpsnoop::%s violated a gated signal. See the Security tab, or set fail-on-findings to false to report without failing.\n' "$session"
		exit 1
	fi
	;;
*)
	# Never suppressible. fail-on-findings answers "should a finding fail the
	# build", and this is the other thing: nothing was checked. A run that could
	# not look is not a run that looked and approved, and silencing it is how a
	# green pipeline comes to mean nothing.
	printf '::error title=mcpsnoop::could not check %s. This is not a finding: nothing was verified. See the step log above.\n' "$session"
	exit 1
	;;
esac
