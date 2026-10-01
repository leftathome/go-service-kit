#!/bin/sh
# retry.sh -- run a DOWNLOAD step, retrying it with backoff when it fails.
#
# The Go gates fetch their modules and their pinned tools (golangci-lint,
# govulncheck) from proxy.golang.org at job time, and on the homelab runners
# that fetch sometimes dies on "net/http: TLS handshake timeout" or a dropped
# connection. Nothing is wrong with the code; a manual retry of the job passes.
# This wrapper is that retry, done once, in one place (quark-q5a).
#
# WRAP ONLY A DOWNLOAD, NEVER A VERDICT. A command whose non-zero exit means
# "your code has a problem" -- a test run, a lint run, a vulnerability scan --
# must not go through here: retrying it turns a finding into a coin toss. The
# Makefile keeps the two apart (download or install the tool HERE, then run it
# ONCE, unwrapped), and scripts/retry_test.go holds it to that.
#
# Usage: scripts/retry.sh <command> [args...]
#
# Bounds, all overridable from the environment:
#   RETRY_ATTEMPTS         attempts in total, including the first      (4)
#   RETRY_DELAY            seconds before the second attempt; tripled
#                          before each later one: 10, 30, 90           (10)
#   RETRY_ATTEMPT_TIMEOUT  seconds one attempt may run before it is
#                          killed and counted as a failure             (600)
#   RETRY_BUDGET           seconds for everything, sleeps included; no
#                          attempt starts or runs past it              (1200)
#
# The two time limits need timeout(1), which every CI image in use has
# (coreutils or busybox). Where it is missing the attempt count still bounds
# the retries, the budget is still checked between attempts, and a line says
# that a hung attempt will not be killed.
#
# Exit status: 0 as soon as an attempt succeeds; otherwise the exit status of
# the LAST attempt, unchanged (124 if that attempt was killed for time). 126
# and 127 (not executable, not found) are returned at once: no amount of
# waiting installs a missing command. 2 for a usage or configuration error.
set -u

usage() {
	echo "usage: retry.sh <command> [args...]" >&2
	exit 2
}

[ $# -ge 1 ] || usage

attempts=${RETRY_ATTEMPTS:-4}
delay=${RETRY_DELAY:-10}
attempt_timeout=${RETRY_ATTEMPT_TIMEOUT:-600}
budget=${RETRY_BUDGET:-1200}

for pair in "RETRY_ATTEMPTS=$attempts" "RETRY_DELAY=$delay" \
	"RETRY_ATTEMPT_TIMEOUT=$attempt_timeout" "RETRY_BUDGET=$budget"; do
	case "${pair#*=}" in
	'' | *[!0-9]*)
		echo "retry: ${pair%%=*} must be a whole number of at least 0, got '${pair#*=}'" >&2
		exit 2
		;;
	esac
done
if [ "$attempts" -lt 1 ] || [ "$attempt_timeout" -lt 1 ] || [ "$budget" -lt 1 ]; then
	echo "retry: RETRY_ATTEMPTS, RETRY_ATTEMPT_TIMEOUT and RETRY_BUDGET must be at least 1" >&2
	exit 2
fi

# --foreground (coreutils) keeps the command in this process group, so an
# interrupt from the terminal still reaches it; busybox has no such flag and
# does not need one.
if ! command -v timeout >/dev/null 2>&1; then
	limiter=''
	echo "retry: no timeout(1) on PATH; a hung attempt will not be killed" >&2
elif timeout --foreground 5 true >/dev/null 2>&1; then
	limiter='timeout --foreground'
else
	limiter='timeout'
fi

rc=1
start=$(date +%s)
n=1
while :; do
	left=$((budget - ($(date +%s) - start)))
	if [ "$left" -lt 1 ]; then
		echo "retry: giving up before attempt $n/$attempts, the ${budget}s budget is spent (exit $rc): $*" >&2
		exit "$rc"
	fi
	limit=$attempt_timeout
	[ "$limit" -le "$left" ] || limit=$left

	rc=0
	if [ -n "$limiter" ]; then
		$limiter "$limit" "$@" || rc=$?
	else
		"$@" || rc=$?
	fi
	[ "$rc" -ne 0 ] || exit 0

	if [ "$rc" -eq 124 ] && [ -n "$limiter" ]; then
		why="killed after ${limit}s"
	else
		why="exit $rc"
	fi
	if [ "$rc" -eq 126 ] || [ "$rc" -eq 127 ]; then
		echo "retry: not retrying, the command cannot be run ($why): $*" >&2
		exit "$rc"
	fi
	if [ "$n" -ge "$attempts" ]; then
		echo "retry: giving up after $n/$attempts attempts ($why): $*" >&2
		exit "$rc"
	fi
	left=$((budget - ($(date +%s) - start)))
	if [ "$delay" -ge "$left" ]; then
		echo "retry: giving up after attempt $n/$attempts ($why), a ${delay}s wait would overrun the ${budget}s budget: $*" >&2
		exit "$rc"
	fi
	echo "retry: attempt $n/$attempts failed ($why), retrying in ${delay}s: $*" >&2
	sleep "$delay"
	delay=$((delay * 3))
	n=$((n + 1))
done
