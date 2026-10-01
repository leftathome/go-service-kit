#!/bin/sh
# retry.sh -- run a DOWNLOAD step, retrying it with backoff when the download
# fails.
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
# WHAT IS RETRIED. A failed attempt is retried only if it looks like a failed
# fetch: it was killed for running too long, or its stderr matches
# RETRY_PATTERN, a list of network error signatures (connection, DNS, TLS and
# timeout errors, HTTP 429 and 5xx). Anything else -- a missing go.sum entry,
# an import that does not resolve, a 404 for a version that does not exist --
# cannot be cured by waiting and fails at once. An unrecognised network error
# therefore fails at once too, which is where things stood before this script
# existed; add its signature to the pattern.
#
# Bounds, all overridable from the environment (whole numbers, no leading
# zero, within the range shown; anything else is refused with exit 2 before
# the command is run):
#   RETRY_ATTEMPTS         attempts in total, including the first
#                          (4; 1..20)
#   RETRY_DELAY            seconds before the second attempt; tripled before
#                          each later one: 10, 30, 90
#                          (10; 0..3600)
#   RETRY_ATTEMPT_TIMEOUT  seconds one attempt may run before it is killed
#                          and counted as a failed fetch
#                          (600; 1..86400)
#   RETRY_BUDGET           seconds for everything, sleeps included; no attempt
#                          starts or runs past it
#                          (1200; 1..86400)
#   RETRY_PATTERN          extended regular expression, matched without regard
#                          to case against the failed attempt's stderr
#
# The two time limits need timeout(1), which every CI image in use has
# (coreutils or busybox). An attempt that outlives its limit gets SIGTERM and,
# if it ignores that, SIGKILL 10 seconds later, so the worst case is the budget
# plus those 10 seconds. Where timeout(1) is missing the attempt count still
# bounds the retries, the budget is still checked between attempts, and a line
# says that a hung attempt will not be killed.
#
# Known limits:
#   - Only the command itself is signalled when an attempt is killed. A
#     process it started (a compiler, a git fetch) is not, and finishes or
#     dies on its own. Killing the whole process group would need the attempt
#     in a group of its own, and then an interrupt from the terminal or a
#     cancelled CI job would no longer reach it.
#   - The command's stderr is shown when the attempt ends, not while it runs:
#     it has to be read to decide whether to retry. stdout is passed through.
#
# Exit status: 0 as soon as an attempt succeeds; otherwise the exit status of
# the LAST attempt, unchanged. For an attempt that was killed that is whatever
# timeout(1) reports -- 124 from coreutils (137 if SIGKILL was needed), 143
# from busybox -- and never 0. 126 and 127 (not executable, not found) are
# returned at once. 2 for a usage or configuration error.
set -u

usage() {
	echo "usage: retry.sh <command> [args...]" >&2
	exit 2
}

[ $# -ge 1 ] || usage

# number NAME VALUE MIN MAX: accept VALUE if it is a plain decimal in MIN..MAX,
# else exit 2. At most five digits and no leading zero, so the shell never sees
# a number it would overflow on or read as octal.
number() {
	case "$2" in
	0 | [1-9] | [1-9][0-9] | [1-9][0-9][0-9] | [1-9][0-9][0-9][0-9] | [1-9][0-9][0-9][0-9][0-9])
		if [ "$2" -ge "$3" ] && [ "$2" -le "$4" ]; then
			return 0
		fi
		;;
	esac
	echo "retry: $1 must be a whole number from $3 to $4 with no leading zero, got '$2'" >&2
	exit 2
}

attempts=${RETRY_ATTEMPTS:-4}
delay=${RETRY_DELAY:-10}
attempt_timeout=${RETRY_ATTEMPT_TIMEOUT:-600}
budget=${RETRY_BUDGET:-1200}
number RETRY_ATTEMPTS "$attempts" 1 20
number RETRY_DELAY "$delay" 0 3600
number RETRY_ATTEMPT_TIMEOUT "$attempt_timeout" 1 86400
number RETRY_BUDGET "$budget" 1 86400

# What a failed fetch looks like on stderr. cmd/go reports a transport error as
# `Get "https://...": <error>` and a bad status as `reading https://...: 502
# Bad Gateway`; the rest are the net, TLS, DNS and HTTP/2 errors underneath,
# and git's own for a module fetched directly. A 404 or 410 is deliberately
# absent: the proxy answered, and the answer will not change.
default_pattern='Get "https?://'
default_pattern="$default_pattern"'|reading https?://[^ ]*: (429|5[0-9][0-9])'
default_pattern="$default_pattern"'|dial tcp|i/o timeout|handshake timeout|handshake failure'
default_pattern="$default_pattern"'|connection (reset|refused|timed out|closed)|broken pipe'
default_pattern="$default_pattern"'|unexpected EOF|: EOF|no such host|server misbehaving'
default_pattern="$default_pattern"'|temporary failure|network is unreachable|no route to host'
default_pattern="$default_pattern"'|context deadline exceeded|Client\.Timeout|stream error|http2:'
default_pattern="$default_pattern"'|bad gateway|service unavailable|gateway timeout|too many requests'
default_pattern="$default_pattern"'|could not resolve host|failed to connect|early EOF|remote end hung up'
default_pattern="$default_pattern"'|returned error: (429|5[0-9][0-9])'
pattern=${RETRY_PATTERN:-$default_pattern}

# Which timeout(1) is this, and can it follow SIGTERM with SIGKILL?
# --foreground (coreutils only) keeps the command in this process group, so an
# interrupt from the terminal or a cancelled job still reaches it.
if ! command -v timeout >/dev/null 2>&1; then
	limiter=''
	echo "retry: no timeout(1) on PATH; a hung attempt will not be killed" >&2
elif timeout --foreground -k 1 5 true >/dev/null 2>&1; then
	limiter='timeout --foreground -k 10'
elif timeout -k 1 5 true >/dev/null 2>&1; then
	limiter='timeout -k 10'
else
	limiter='timeout -s KILL'
fi

tmp=$(mktemp -d) || exit 2
trap 'rm -rf "$tmp"' EXIT

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

	began=$(date +%s)
	rc=0
	if [ -n "$limiter" ]; then
		# shellcheck disable=SC2086 # $limiter is a command plus its flags
		$limiter "$limit" "$@" 2>"$tmp/stderr" || rc=$?
	else
		"$@" 2>"$tmp/stderr" || rc=$?
	fi
	cat "$tmp/stderr" >&2

	# Killed for time? Not every timeout(1) says so in its exit status (busybox
	# passes on the command's own, which is 0 if it caught SIGTERM and exited
	# cleanly), so the clock decides; coreutils does say so, and there its
	# 124 or 137 is required as well, so that a command which simply finished
	# on the last second is not called killed. A status of 0 from a killed
	# attempt is a failure, never a success.
	killed=''
	if [ -n "$limiter" ] && [ $(($(date +%s) - began)) -ge "$limit" ]; then
		case "$limiter:$rc" in
		*--foreground*:124 | *--foreground*:137) killed=yes ;;
		*--foreground*) ;;
		*) killed=yes ;;
		esac
	fi
	if [ -n "$killed" ]; then
		[ "$rc" -ne 0 ] || rc=124
		why="killed after ${limit}s, exit $rc"
	else
		[ "$rc" -ne 0 ] || exit 0
		why="exit $rc"
	fi

	if [ "$rc" -eq 126 ] || [ "$rc" -eq 127 ]; then
		echo "retry: not retrying, the command cannot be run ($why): $*" >&2
		exit "$rc"
	fi
	if [ -z "$killed" ] && ! grep -E -i -q -e "$pattern" "$tmp/stderr"; then
		echo "retry: not retrying, this does not look like a failed download ($why): $*" >&2
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
