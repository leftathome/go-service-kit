// Package scripts holds the tests for the shell helpers in this directory and
// for the Makefile wiring that uses them. There is no Go code here to import.
//
// Two things are under test (quark-q5a):
//
//   - retry.sh itself: a download that fails and then succeeds is a success, one
//     that never succeeds fails with its own exit status after a bounded number
//     of attempts, a failure that is not a download is not retried at all, and
//     the time limits hold.
//   - the Makefile's use of it: the DOWNLOAD of a tool is retried, the tool's
//     VERDICT never is. Those tests run the real Makefile against a fake `go`
//     that fails on demand and installs a fake tool that records every call.
//
// This file writes executable fixtures and runs them, which is everything
// gosec exists to flag; .golangci.yml excludes gosec for this one file rather
// than have it carry a nolint directive per call.
//
// Unix only: everything here drives sh and make, and the signal tests need
// kill(2).
//
// NO TEST HERE MAY DEPEND ON HOW FAST THE MACHINE IS. These tests run on CI
// nodes where starting a process can take seconds, twice at once in the
// template (its own copy and the generated project's). So:
//   - nothing has to FINISH within a limit: an attempt that is meant to be
//     killed never ends by itself, and one that is meant to end by itself runs
//     under a limit of minutes;
//   - what is asserted is what happened (log lines, exit status, attempt
//     count, which process is alive), not how long it took;
//   - where "promptly" is the property, the bound is far below what a wrong
//     implementation would take (stopBound against a 600s sleep), and far
//     above what a loaded machine needs;
//   - the one rule that is about the clock itself is tested with a fake
//     date(1), not by racing the real one.

//go:build unix

package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A network failure as cmd/go prints it: the request that failed in the
// incident this all started from.
const fetchFailure = `go: golang.org/x/vuln/cmd/govulncheck@v1.6.0: Get "https://proxy.golang.org/golang.org/x/vuln/cmd/govulncheck/@v/v1.6.0.info": net/http: TLS handshake timeout`

// flaky is a command that fails with status $2 until it has been run more
// than $1 times, counting its runs in ./runs. A failing run prints $FLAKY_MSG
// (default: a fetch failure) to stderr -- unless a third argument says how to
// overstay instead, in which case it never ends by itself (600s, far beyond
// any limit a test sets): "hang" sleeps, "stubborn" sleeps ignoring SIGTERM,
// "polite" creates ./ready once it is prepared to exit 0 on SIGTERM and then
// sleeps, "chatty" writes a line to stderr and its pid to ./pid and then
// sleeps.
const flaky = `#!/bin/sh
echo run >>runs
n=$(wc -l <runs)
[ "$n" -gt "$1" ] && exit 0
case "${3:-}" in
hang) exec sleep 600 ;;
stubborn)
	trap '' TERM
	exec sleep 600
	;;
polite)
	trap 'exit 0' TERM
	: >ready
	i=0
	while [ "$i" -lt 6000 ]; do
		sleep 0.1
		i=$((i + 1))
	done
	;;
chatty)
	echo "flaky: partial progress" >&2
	echo $$ >pid
	exec sleep 600
	;;
esac
echo "${FLAKY_MSG:-$FLAKY_DEFAULT_MSG}" >&2
exit "$2"
`

// fakeSleep stands in for sleep(1) so the backoff can be read without being
// waited for.
const fakeSleep = `#!/bin/sh
echo "$1" >>"$SLEEP_LOG"
`

// fakeGo stands in for the go command under make. Every call is appended to
// $FAKE_DIR/go.log.
//
//   - `go env GOVERSION|GOHOSTOS|GOHOSTARCH` prints $FAKE_DIR/goversion, goos
//     or goarch (defaults go1.26.6, linux, amd64).
//   - `go version -m <binary>` prints what a successful install recorded next
//     to that binary, in the real command's format.
//   - any other `go <verb>` fails while $FAKE_DIR/<verb>.fails holds a number
//     above zero (counting it down), printing $FAKE_DIR/<verb>.msg (default: a
//     fetch failure); then it exits with $FAKE_DIR/<verb>.exit (default 0).
//   - a successful `go install pkg@version` writes a fake tool into $GOBIN that
//     logs its own calls to $FAKE_DIR/tool.log and exits with
//     $FAKE_DIR/tool.exit. Like the real command it leaves a file that is
//     already there ALONE (go decides "up to date" from its build cache, not
//     from the file), so a damaged tool is only repaired if the Makefile
//     removed it first.
//
// A failing VERDICT (the tool, or go vet / go test via <verb>.exit) also prints
// a fetch-failure line. That is the worst case for the rule under test: a real
// scan can die mentioning the network, and put behind the retry it WOULD be
// retried. The run counts in the tests below catch exactly that.
const fakeGo = `#!/bin/sh
echo "go $*" >>"$FAKE_DIR/go.log"
verb=$1
gover=$(cat "$FAKE_DIR/goversion" 2>/dev/null || echo go1.26.6)
goos=$(cat "$FAKE_DIR/goos" 2>/dev/null || echo linux)
goarch=$(cat "$FAKE_DIR/goarch" 2>/dev/null || echo amd64)
case "$verb" in
env)
	case "$2" in
	GOVERSION) echo "$gover" ;;
	GOHOSTOS) echo "$goos" ;;
	GOHOSTARCH) echo "$goarch" ;;
	*) exit 1 ;;
	esac
	exit 0
	;;
version)
	cat "$3.meta" 2>/dev/null
	exit
	;;
esac
left=$(cat "$FAKE_DIR/$verb.fails" 2>/dev/null || echo 0)
if [ "$left" -gt 0 ]; then
	echo $((left - 1)) >"$FAKE_DIR/$verb.fails"
	cat "$FAKE_DIR/$verb.msg" >&2 2>/dev/null || echo "$FAKE_DEFAULT_MSG" >&2
	exit 1
fi
if [ "$verb" = install ]; then
	pkg=${2%@*}
	name=${pkg##*/}
	mkdir -p "$GOBIN"
	if [ ! -e "$GOBIN/$name" ]; then
		cat >"$GOBIN/$name" <<TOOL
#!/bin/sh
echo "$name \$*" >>"$FAKE_DIR/tool.log"
rc=\$(cat "$FAKE_DIR/tool.exit" 2>/dev/null || echo 0)
[ "\$rc" = 0 ] || echo "\$FAKE_DEFAULT_MSG" >&2
exit "\$rc"
TOOL
		chmod +x "$GOBIN/$name"
	fi
	printf '%s: %s\n\tpath\t%s\n\tmod\t%s\t%s\th1:fake=\n\tbuild\t-buildmode=exe\n\tbuild\tGOARCH=%s\n\tbuild\tGOOS=%s\n' \
		"$GOBIN/$name" "$gover" "$pkg" "$pkg" "${2##*@}" "$goarch" "$goos" >"$GOBIN/$name.meta"
fi
rc=$(cat "$FAKE_DIR/$verb.exit" 2>/dev/null || echo 0)
[ "$rc" = 0 ] || echo "$FAKE_DEFAULT_MSG" >&2
exit "$rc"
`

// fakeLint is a golangci-lint found on PATH: it logs like an installed tool.
const fakeLint = `#!/bin/sh
echo "golangci-lint $*" >>"$FAKE_DIR/tool.log"
rc=$(cat "$FAKE_DIR/tool.exit" 2>/dev/null || echo 0)
[ "$rc" = 0 ] || echo "$FAKE_DEFAULT_MSG" >&2
exit "$rc"
`

// passThroughTimeout is a timeout(1) of the kind that reports the COMMAND's
// exit status when it kills it (busybox with -k behaves so): no --foreground,
// SIGTERM at the limit, and whatever the command then exits with. It does not
// send the signal before ./ready exists, so that the command it kills is
// certain to have got as far as it needs to, however slow the machine.
const passThroughTimeout = `#!/bin/sh
[ "$1" = --foreground ] && exit 1
[ "$1" = -k ] && shift 2
secs=$1
shift
"$@" &
pid=$!
(
	sleep "$secs"
	while [ ! -e ready ]; do sleep 0.1; done
	kill -TERM "$pid" 2>/dev/null
) >/dev/null 2>&1 &
wait "$pid"
`

// untimedTimeout takes coreutils timeout's arguments and enforces nothing: the
// command runs to its own end and its status is passed on. retry.sh takes it
// for coreutils (it accepts --foreground), and no real timer can fire.
const untimedTimeout = `#!/bin/sh
[ "$1" = --foreground ] && shift
[ "$1" = -k ] && shift 2
shift
exec "$@"
`

// fakeClock is a date(1) whose every reading is 1000 seconds after the last
// one, kept in $CLOCK. By this clock everything takes longer than any limit.
const fakeClock = `#!/bin/sh
n=$(cat "$CLOCK" 2>/dev/null || echo 0)
n=$((n + 1000))
echo "$n" >"$CLOCK"
echo "$n"
`

// stopBound is how long something that must happen "at once" may take. The
// alternative it is told apart from is always a 600s sleep; a loaded CI node
// needs seconds.
const stopBound = 2 * time.Minute

// safetyNet bounds a run that should simply end: a hang fails the test with
// its output instead of timing the whole package out.
const safetyNet = 5 * time.Minute

func needShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
}

func retryScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("retry.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// lines returns the non-empty lines of a log file, or nil if it is absent.
func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func count(ls []string, prefix string) int {
	n := 0
	for _, l := range ls {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("command did not run: %v", err)
	}
	return ee.ExitCode()
}

// start starts cmd in a process group of its own, with stdout and stderr going
// to the file it returns the path of -- a file, not a pipe, so that a process
// left behind cannot keep a reader waiting. Whatever is still in the group
// when the test ends is killed.
func start(t *testing.T, cmd *exec.Cmd) (outPath string) {
	t.Helper()
	outPath = filepath.Join(t.TempDir(), "output")
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close() // the child has its own descriptors
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return outPath
}

// waitWithin waits for a started cmd and returns its exit status, or fails the
// test if it has not ended within bound (and kills its process group).
func waitWithin(t *testing.T, cmd *exec.Cmd, bound time.Duration, outPath string) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(t, err)
	case <-time.After(bound):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("still running after %s:\n%s", bound, strings.Join(lines(t, outPath), "\n"))
		return -1
	}
}

// runRetryWithin runs retry.sh in a fresh directory holding the flaky command,
// with env added to the environment, and returns its exit status, its combined
// output and that directory. It fails the test if the run takes longer than
// bound.
func runRetryWithin(t *testing.T, bound time.Duration, env []string, args ...string) (code int, out, dir string) {
	t.Helper()
	needShell(t)
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "flaky"), flaky, 0o700)
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{retryScript(t)}, args...)...)
	cmd.Dir = dir
	// Blank any RETRY_* setting this process inherited, so only env decides.
	cmd.Env = append(os.Environ(),
		"RETRY_ATTEMPTS=", "RETRY_DELAY=", "RETRY_ATTEMPT_TIMEOUT=", "RETRY_BUDGET=", "RETRY_PATTERN=",
		"FLAKY_MSG=", "FLAKY_DEFAULT_MSG="+fetchFailure)
	cmd.Env = append(cmd.Env, env...)
	outPath := start(t, cmd)
	code = waitWithin(t, cmd, bound, outPath)
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(raw), dir
}

// runRetry is runRetryWithin for a run that should simply end.
func runRetry(t *testing.T, env []string, args ...string) (code int, out, dir string) {
	t.Helper()
	return runRetryWithin(t, safetyNet, env, args...)
}

func runs(t *testing.T, dir string) int {
	t.Helper()
	return len(lines(t, filepath.Join(dir, "runs")))
}

func TestRetrySuccessRunsOnce(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=0"}, "./flaky", "0", "1")
	if code != 0 || runs(t, dir) != 1 {
		t.Fatalf("exit %d after %d runs, want 0 after 1\n%s", code, runs(t, dir), out)
	}
	if strings.Contains(out, "retry:") {
		t.Errorf("a first-time success must log nothing, got:\n%s", out)
	}
}

func TestRetryFailingThenSucceedingSucceeds(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=0"}, "./flaky", "2", "1")
	if code != 0 || runs(t, dir) != 3 {
		t.Fatalf("exit %d after %d runs, want 0 after 3\n%s", code, runs(t, dir), out)
	}
	for _, want := range []string{
		"retry: attempt 1/4 failed (exit 1), retrying in 0s: ./flaky 2 1",
		"retry: attempt 2/4 failed (exit 1), retrying in 0s: ./flaky 2 1",
		fetchFailure, // the command's own stderr is still shown
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing log line %q in:\n%s", want, out)
		}
	}
}

func TestRetryAlwaysFailingKeepsTheExitStatus(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=0", "RETRY_ATTEMPTS=3"}, "./flaky", "99", "7")
	if code != 7 {
		t.Errorf("exit %d, want the command's own 7\n%s", code, out)
	}
	if runs(t, dir) != 3 {
		t.Errorf("%d runs, want exactly RETRY_ATTEMPTS=3\n%s", runs(t, dir), out)
	}
	if want := "retry: giving up after 3/3 attempts (exit 7): ./flaky 99 7"; !strings.Contains(out, want) {
		t.Errorf("missing log line %q in:\n%s", want, out)
	}
}

// The defaults, read off a fake sleep: four attempts, waiting 10s, 30s, 90s.
func TestRetryDefaultBackoff(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "sleep"), fakeSleep, 0o700)
	sleepLog := filepath.Join(bin, "sleep.log")
	code, out, dir := runRetry(t, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SLEEP_LOG=" + sleepLog,
	}, "./flaky", "99", "1")
	if code != 1 || runs(t, dir) != 4 {
		t.Fatalf("exit %d after %d runs, want 1 after 4\n%s", code, runs(t, dir), out)
	}
	if got := strings.Join(lines(t, sleepLog), " "); got != "10 30 90" {
		t.Errorf("slept %q, want \"10 30 90\"\n%s", got, out)
	}
}

// Only a failure that looks like a failed fetch is worth waiting on. Each
// message is what the always-failing command prints; it is retried or it
// fails at once. The retried ones are one sample per alternative of the
// default pattern, each chosen to match that alternative and no other, so that
// losing any one of them fails here.
func TestRetryOnlyRetriesWhatLooksLikeAFailedDownload(t *testing.T) {
	t.Parallel()
	retried := map[string]string{
		"the recorded incident":     fetchFailure,
		"Get, nothing else":         `go: example.com/m@v1.0.0: Get "https://proxy.golang.org/example.com/m/@v/v1.0.0.mod": net/http: request canceled`,
		"read, body phase":          `go: example.com/m@v1.0.0: read "https://proxy.golang.org/example.com/m/@v/v1.0.0.zip": stream reset`,
		"proxy 502":                 `go: example.com/m@v1.0.0: reading https://proxy.golang.org/example.com/m/@v/v1.0.0.zip: 502`,
		"proxy 429":                 `go: example.com/m@v1.0.0: reading https://proxy.golang.org/example.com/m/@v/v1.0.0.zip: 429`,
		"proxy 408":                 `go: example.com/m@v1.0.0: reading https://proxy.golang.org/example.com/m/@v/v1.0.0.zip: 408`,
		"git 503":                   `error: The requested URL returned error: 503`,
		"git 408":                   `error: The requested URL returned error: 408`,
		"dial":                      `dial tcp 142.250.0.1:443: connect: operation not permitted`,
		"i/o timeout":               `read udp 10.0.0.1:5353: i/o timeout`,
		"TLS handshake timeout":     `net/http: TLS handshake timeout`,
		"TLS handshake failure":     `remote error: tls: handshake failure`,
		"connection reset":          `read: connection reset by peer`,
		"connection refused":        `connect: connection refused`,
		"connection timed out":      `connect: connection timed out`,
		"connection closed":         `http: server closed idle connection closed`,
		"broken pipe":               `write: broken pipe`,
		"closed network connection": `use of closed network connection`,
		"bad record MAC":            `local error: tls: bad record MAC`,
		"truncated body":            `go: downloading example.com/m v1.0.0: unexpected EOF`,
		"bare EOF":                  `go: example.com/m@v1.0.0: EOF`,
		"DNS, no such host":         `lookup proxy.golang.org: no such host`,
		"DNS, server misbehaving":   `lookup proxy.golang.org on 10.96.0.10:53: server misbehaving`,
		"DNS, temporary failure":    `Temporary failure in name resolution`,
		"network unreachable":       `connect: network is unreachable`,
		"no route":                  `connect: no route to host`,
		"deadline":                  `context deadline exceeded`,
		"client timeout":            `(Client.Timeout exceeded while awaiting headers)`,
		"HTTP/2 stream error":       `stream error: stream ID 7; INTERNAL_ERROR`,
		"HTTP/2":                    `http2: server sent GOAWAY and closed`,
		"status text, 408":          `408 Request Timeout`,
		"status text, 502":          `Bad Gateway`,
		"status text, 503":          `Service Unavailable`,
		"status text, 504":          `Gateway Timeout`,
		"status text, 429":          `Too Many Requests`,
		"corrupt zip":               `go: example.com/m@v1.0.0: zip: not a valid zip file`,
		"git cannot resolve":        `fatal: unable to access 'https://example.com/m/': Could not resolve host: example.com`,
		"curl cannot connect":       `fatal: unable to access 'https://example.com/m/': Failed to connect to example.com port 443`,
		"git early EOF":             `fatal: early EOF`,
		"git hung up":               `fatal: the remote end hung up unexpectedly`,
		"git gnutls":                `fatal: unable to access 'https://example.com/m/': gnutls_handshake() failed: Error in the pull function.`,
		"git RPC":                   `error: RPC failed; HTTP 000`,
		"curl 56":                   `error: curl 56 GnuTLS recv error (-54)`,
		"curl 18":                   `error: curl 18 transfer closed with outstanding read data remaining`,
		"RETRY_PATTERN widens it":   `missing go.sum entry for module providing package example.com/m`,
	}
	failsAtOnce := map[string]string{
		"missing go.sum entry":         `missing go.sum entry for module providing package example.com/m; to add it: go get example.com/m`,
		"cross-compiled install":       `go: cannot install cross-compiled binaries when GOBIN is set`,
		"version that does not exist":  `go: example.com/m@v9.9.9: reading https://proxy.golang.org/example.com/m/@v/v9.9.9.info: 404 Not Found`,
		"version that is gone":         `go: example.com/m@v9.9.9: reading https://proxy.golang.org/example.com/m/@v/v9.9.9.info: 410 Gone`,
		"import that does not resolve": `package example.com/nope is not in std`,
		"compile error in the tool":    `./main.go:3:1: syntax error: non-declaration statement outside function body`,
		"says nothing at all":          ` `,
	}
	run := func(name, msg, pattern string, wantRuns int) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out, dir := runRetry(t, []string{
				"RETRY_DELAY=0", "RETRY_ATTEMPTS=2", "FLAKY_MSG=" + msg, "RETRY_PATTERN=" + pattern,
			}, "./flaky", "99", "5")
			if code != 5 || runs(t, dir) != wantRuns {
				t.Errorf("exit %d after %d runs, want 5 after %d\n%s", code, runs(t, dir), wantRuns, out)
			}
			if wantRuns == 1 && !strings.Contains(out, "retry: not retrying, this does not look like a failed download (exit 5)") {
				t.Errorf("missing the not-retrying line in:\n%s", out)
			}
		})
	}
	for name, msg := range retried {
		pattern := ""
		if name == "RETRY_PATTERN widens it" {
			pattern = `go\.sum`
		}
		run("retried/"+name, msg, pattern, 2)
	}
	for name, msg := range failsAtOnce {
		run("fails at once/"+name, msg, "", 1)
	}
}

// A checksum mismatch is a finding about the supply chain. It is never
// retried: not when the same output also carries a network error, and not when
// RETRY_PATTERN would match it.
func TestRetryNeverRetriesAChecksumMismatch(t *testing.T) {
	t.Parallel()
	mismatch := "verifying example.com/m@v1.0.0: checksum mismatch\n\tdownloaded: h1:AAAA\n\tgo.sum:     h1:BBBB\n\nSECURITY ERROR\nThis download does NOT match an earlier download recorded in go.sum."
	for name, tc := range map[string]struct{ msg, pattern string }{
		"go.sum mismatch":                 {msg: mismatch},
		"checksum database mismatch":      {msg: "verifying module: checksum mismatch\n\tdownloaded: h1:AAAA\n\tsum.golang.org: h1:BBBB"},
		"the SECURITY ERROR banner alone": {msg: "SECURITY ERROR"},
		"with a network error beside it":  {msg: fetchFailure + "\n" + mismatch},
		"with RETRY_PATTERN matching it":  {msg: mismatch, pattern: "checksum|verifying"},
		"with RETRY_PATTERN matching all": {msg: mismatch, pattern: "."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out, dir := runRetry(t, []string{
				"RETRY_DELAY=0", "RETRY_ATTEMPTS=3", "FLAKY_MSG=" + tc.msg, "RETRY_PATTERN=" + tc.pattern,
			}, "./flaky", "99", "1")
			if code != 1 || runs(t, dir) != 1 {
				t.Errorf("exit %d after %d runs, want 1 after exactly 1\n%s", code, runs(t, dir), out)
			}
			if !strings.Contains(out, "retry: not retrying, a checksum mismatch is a finding, not a failed download (exit 1)") {
				t.Errorf("missing the checksum line in:\n%s", out)
			}
		})
	}
}

// stdout is the command's and passes through untouched; only stderr is
// captured, and it comes back out on stderr.
func TestRetryKeepsStdoutApartFromStderr(t *testing.T) {
	t.Parallel()
	needShell(t)
	cmd := exec.CommandContext(t.Context(), "sh", retryScript(t), "sh", "-c", "echo to-stdout; echo to-stderr >&2")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "to-stdout\n" || stderr.String() != "to-stderr\n" {
		t.Errorf("stdout %q, stderr %q; want \"to-stdout\\n\" and \"to-stderr\\n\"", stdout.String(), stderr.String())
	}
}

// With coreutils timeout a kill is reported, so an attempt that fails by
// itself -- however late, even after its limit by the clock -- is an ordinary
// failure: it is not called killed, and so it is not retried on the strength
// of having been slow.
//
// Late BY THE CLOCK is arranged, not raced for: date(1) is a fake that jumps
// 1000 seconds at every reading, and timeout(1) is a fake that takes coreutils'
// arguments and never fires. The command fails at once, on its own account,
// and by retry.sh's arithmetic it overstayed a 600s limit.
func TestRetryDoesNotCallASlowFailureAKill(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "date"), fakeClock, 0o700)
	writeFile(t, filepath.Join(bin, "timeout"), untimedTimeout, 0o700)
	code, out, dir := runRetry(t, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CLOCK=" + filepath.Join(bin, "clock"),
		"RETRY_DELAY=0", "RETRY_ATTEMPTS=2", "RETRY_BUDGET=86400", "FLAKY_MSG=not a download problem",
	}, "./flaky", "99", "5")
	if code != 5 || runs(t, dir) != 1 {
		t.Errorf("exit %d after %d runs, want the command's own 5 after 1\n%s", code, runs(t, dir), out)
	}
	if strings.Contains(out, "killed") || !strings.Contains(out, "this does not look like a failed download (exit 5)") {
		t.Errorf("a command that failed by itself was called killed:\n%s", out)
	}
}

// An interrupt is acted on at once, during an attempt and during the wait
// between two: the attempt is stopped, the stderr captured so far is shown
// (once), the temp directory is removed, nothing further is attempted, and the
// exit status is the conventional 128+signal.
//
// "At once" means: without sitting out the attempt or the backoff, which are
// both 600s here. The test allows stopBound.
func TestRetryStopsAtOnceWhenInterrupted(t *testing.T) {
	t.Parallel()
	needShell(t)
	for name, tc := range map[string]struct {
		sig  syscall.Signal
		code int
	}{
		"SIGTERM": {syscall.SIGTERM, 143},
		"SIGINT":  {syscall.SIGINT, 130},
		"SIGHUP":  {syscall.SIGHUP, 129},
	} {
		for _, phase := range []string{"during an attempt", "during the backoff"} {
			t.Run(name+" "+phase, func(t *testing.T) {
				t.Parallel()
				if signal.Ignored(tc.sig) {
					t.Skipf("%s is ignored in this process, so a shell started from it cannot trap it", name)
				}
				dir := t.TempDir()
				tmp := filepath.Join(dir, "tmp")
				writeFile(t, filepath.Join(tmp, ".keep"), "", 0o600)
				writeFile(t, filepath.Join(dir, "flaky"), flaky, 0o700)

				mode := "chatty" // sleeps 600s with a line of stderr captured
				if phase == "during the backoff" {
					mode = "" // fails at once with a fetch failure; then a 600s wait
				}
				cmd := exec.CommandContext(t.Context(), "sh", retryScript(t), "./flaky", "99", "1", mode)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "RETRY_ATTEMPTS=3", "RETRY_DELAY=600",
					"RETRY_ATTEMPT_TIMEOUT=", "RETRY_BUDGET=", "RETRY_PATTERN=", "FLAKY_MSG=", "FLAKY_DEFAULT_MSG="+fetchFailure)
				outPath := start(t, cmd)

				// Wait for the state to interrupt, by what retry.sh and the
				// command have written, for as long as it takes.
				ready := func() bool {
					if mode == "chatty" {
						return len(lines(t, filepath.Join(dir, "pid"))) == 1
					}
					return strings.Contains(strings.Join(lines(t, outPath), "\n"), "retrying in 600s")
				}
				for deadline := time.Now().Add(safetyNet); !ready(); time.Sleep(20 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("retry.sh never got as far as %s:\n%s", phase, strings.Join(lines(t, outPath), "\n"))
					}
				}
				// The command writes its pid a moment before retry.sh has noted
				// that pid and begun to wait. A second is enough for those two
				// shell statements; nothing below depends on the second being
				// short.
				time.Sleep(time.Second)

				if err := cmd.Process.Signal(tc.sig); err != nil {
					t.Fatal(err)
				}
				code := waitWithin(t, cmd, stopBound, outPath)
				out := strings.Join(lines(t, outPath), "\n")

				if code != tc.code {
					t.Errorf("exit %d, want %d\n%s", code, tc.code, out)
				}
				if runs(t, dir) != 1 {
					t.Errorf("%d attempts were started, want 1\n%s", runs(t, dir), out)
				}
				left, err := os.ReadDir(tmp)
				if err != nil {
					t.Fatal(err)
				}
				if len(left) != 1 { // .keep
					t.Errorf("the temp directory was left behind: %v", left)
				}
				if mode == "chatty" {
					if strings.Count(out, "flaky: partial progress") != 1 {
						t.Errorf("the captured stderr was not shown exactly once:\n%s", out)
					}
					pid := 0
					for _, c := range lines(t, filepath.Join(dir, "pid"))[0] {
						pid = pid*10 + int(c-'0')
					}
					gone := false
					for deadline := time.Now().Add(stopBound); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
						if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
							gone = true
							break
						}
					}
					if !gone {
						t.Errorf("the attempt (pid %d) was left running", pid)
					}
				} else if strings.Count(out, fetchFailure) != 1 {
					t.Errorf("the failed attempt's stderr was shown %d times, want once:\n%s", strings.Count(out, fetchFailure), out)
				}
			})
		}
	}
}

func TestRetryDoesNotRetryAMissingCommand(t *testing.T) {
	t.Parallel()
	code, out, _ := runRetry(t, []string{"RETRY_DELAY=0"}, "./no-such-command")
	if code != 126 && code != 127 {
		t.Errorf("exit %d, want 126 or 127\n%s", code, out)
	}
	if strings.Contains(out, "retrying in") || !strings.Contains(out, "retry: not retrying, the command cannot be run") {
		t.Errorf("a missing command must fail at once, got:\n%s", out)
	}
}

// timeoutFlavors returns a PATH for each timeout(1) available here: the one
// already on PATH (coreutils on the CI images) and busybox's, which reports a
// killed command differently and is what an alpine-based image would have.
func timeoutFlavors(t *testing.T) map[string]string {
	t.Helper()
	flavors := map[string]string{}
	if _, err := exec.LookPath("timeout"); err == nil {
		flavors["timeout on PATH"] = os.Getenv("PATH")
	}
	if bb, err := exec.LookPath("busybox"); err == nil {
		dir := t.TempDir()
		if err := os.Symlink(bb, filepath.Join(dir, "timeout")); err != nil {
			t.Fatal(err)
		}
		flavors["busybox timeout"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	if len(flavors) == 0 {
		t.Skip("no timeout(1) here; retry.sh cannot bound a hung attempt")
	}
	return flavors
}

// An attempt that overstays its limit is killed and counted as a failed
// fetch, whatever it does about the signal: it is retried, it is never
// reported as a success, and the log says it was killed.
//
// The command never ends by itself, on any attempt, so nothing here has to
// beat the 1s limit: the assertions are on the two log lines and the status.
// (A machine too slow to get the command as far as its trap within the second
// still kills it, and the test still passes; it only exercises less.)
func TestRetryKillsAnAttemptThatOverstays(t *testing.T) {
	t.Parallel()
	for flavor, path := range timeoutFlavors(t) {
		for mode, attempts := range map[string]int{
			"hang":     2, // dies on SIGTERM
			"polite":   2, // exits 0 on SIGTERM: still a failure, still retried
			"stubborn": 1, // ignores SIGTERM: SIGKILL follows, 10s later
		} {
			t.Run(flavor+"/"+mode, func(t *testing.T) {
				t.Parallel()
				// stopBound: without the SIGKILL, "stubborn" would sleep out
				// its 600s.
				code, out, _ := runRetryWithin(t, stopBound, []string{
					"PATH=" + path, "RETRY_DELAY=0", "RETRY_ATTEMPT_TIMEOUT=1", "RETRY_ATTEMPTS=" + strconv.Itoa(attempts),
				}, "./flaky", "99", "1", mode)
				if code == 0 {
					t.Errorf("exit 0 although every attempt was killed\n%s", out)
				}
				killed := `\(killed after 1s, exit [1-9][0-9]*\)`
				want := []string{`giving up after ` + strconv.Itoa(attempts) + `/` + strconv.Itoa(attempts) + ` attempts ` + killed}
				if attempts == 2 {
					want = append(want, `attempt 1/2 failed `+killed+`, retrying in 0s`)
				}
				for _, w := range want {
					if !regexp.MustCompile(w).MatchString(out) {
						t.Errorf("no line matching %q: the overstaying attempt was not reported as killed with a failing status, or not retried:\n%s", w, out)
					}
				}
			})
		}
	}
}

// A killed LAST attempt must fail the step even if the command exited 0 on
// its way out. The third timeout here is the one that makes that happen for
// certain: it passes the command's own status on, and it does not send the
// signal until the command is ready to answer it with exit 0.
func TestRetryNeverReportsAKilledAttemptAsSuccess(t *testing.T) {
	t.Parallel()
	flavors := timeoutFlavors(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "timeout"), passThroughTimeout, 0o700)
	flavors["timeout that passes the command's status on"] = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	for flavor, path := range flavors {
		t.Run(flavor, func(t *testing.T) {
			t.Parallel()
			code, out, _ := runRetryWithin(t, stopBound, []string{"PATH=" + path, "RETRY_ATTEMPTS=1", "RETRY_ATTEMPT_TIMEOUT=1"},
				"./flaky", "99", "1", "polite")
			if code == 0 {
				t.Errorf("exit 0 from a killed attempt\n%s", out)
			}
			if !regexp.MustCompile(`giving up after 1/1 attempts \(killed after 1s, exit [1-9][0-9]*\)`).MatchString(out) {
				t.Errorf("missing the killed line in:\n%s", out)
			}
		})
	}
}

// 124 is what coreutils timeout returns for a kill, but a command may exit
// 124 on its own account, and then nothing was killed.
func TestRetryReportsACommandsOwn124AsAnExit(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=0", "RETRY_ATTEMPTS=2"}, "./flaky", "99", "124")
	if code != 124 || runs(t, dir) != 2 {
		t.Fatalf("exit %d after %d runs, want 124 after 2\n%s", code, runs(t, dir), out)
	}
	if strings.Contains(out, "killed") || !strings.Contains(out, "giving up after 2/2 attempts (exit 124)") {
		t.Errorf("a command's own exit 124 was reported as a kill:\n%s", out)
	}
}

// The budget bounds the whole thing: a wait that would overrun it is not
// started, and the status is still the command's own. The wait (an hour) is
// longer than the whole budget (100s), so it overruns however little of the
// budget the first attempt used -- and the attempt itself has all 100s.
func TestRetryStopsAtTheBudget(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=3600", "RETRY_BUDGET=100"}, "./flaky", "99", "9")
	if code != 9 || runs(t, dir) != 1 {
		t.Fatalf("exit %d after %d runs, want 9 after 1\n%s", code, runs(t, dir), out)
	}
	if !strings.Contains(out, "giving up after attempt 1/4 (exit 9), a 3600s wait would overrun the 100s budget") {
		t.Errorf("missing the budget line in:\n%s", out)
	}
}

// A setting the shell could misread -- octal, overflow, not a number -- is
// refused before the command is run, never half-honoured.
func TestRetryRejectsBadSettings(t *testing.T) {
	t.Parallel()
	huge := "99999999999999999999999"
	for _, setting := range []string{
		"RETRY_ATTEMPTS=many", "RETRY_ATTEMPTS=0", "RETRY_ATTEMPTS=-1", "RETRY_ATTEMPTS= 3", "RETRY_ATTEMPTS=1e3",
		"RETRY_ATTEMPTS=08", "RETRY_ATTEMPTS=21", "RETRY_ATTEMPTS=" + huge,
		"RETRY_DELAY=-1", "RETRY_DELAY=08", "RETRY_DELAY=010", "RETRY_DELAY=3601", "RETRY_DELAY=" + huge,
		"RETRY_ATTEMPT_TIMEOUT=0", "RETRY_ATTEMPT_TIMEOUT=09", "RETRY_ATTEMPT_TIMEOUT=86401", "RETRY_ATTEMPT_TIMEOUT=" + huge,
		"RETRY_BUDGET=0", "RETRY_BUDGET=08", "RETRY_BUDGET=86401", "RETRY_BUDGET=" + huge,
	} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			code, out, dir := runRetry(t, []string{setting}, "./flaky", "0", "1")
			if code != 2 || runs(t, dir) != 0 {
				t.Errorf("exit %d after %d runs, want 2 after 0\n%s", code, runs(t, dir), out)
			}
			name, _, _ := strings.Cut(setting, "=")
			if !strings.Contains(out, "retry: "+name+" must be a whole number from ") {
				t.Errorf("the refusal does not name %s:\n%s", name, out)
			}
		})
	}
}

func TestRetryRejectsNoCommand(t *testing.T) {
	t.Parallel()
	code, out, _ := runRetry(t, nil)
	if code != 2 || !strings.Contains(out, "usage: retry.sh") {
		t.Errorf("exit %d, want 2 and the usage line\n%s", code, out)
	}
}

// The limits of each range are themselves accepted.
func TestRetryAcceptsTheLimitsOfEachRange(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{
		"RETRY_ATTEMPTS=20", "RETRY_DELAY=3600", "RETRY_ATTEMPT_TIMEOUT=86400", "RETRY_BUDGET=86400",
	}, "./flaky", "0", "1")
	if code != 0 || runs(t, dir) != 1 {
		t.Errorf("exit %d after %d runs, want 0 after 1\n%s", code, runs(t, dir), out)
	}
}

func TestRetryPassesArgumentsThrough(t *testing.T) {
	t.Parallel()
	code, out, _ := runRetry(t, nil, "printf", "[%s]", "a b", "", "c")
	if code != 0 || out != "[a b][][c]" {
		t.Errorf("exit %d, output %q; want 0 and \"[a b][][c]\"", code, out)
	}
}

// makeEnv is a scratch copy of the Makefile and retry.sh with a fake
// toolchain beside it. Its PATH holds ONLY the fake go and the handful of
// system commands the recipes use, so neither the real toolchain nor a
// golangci-lint installed on this machine can take part. The copy lives in a
// directory with a space in its name: every run doubles as a check that no
// recipe splits a path.
type makeEnv struct {
	t                *testing.T
	makeBin          string
	work, bin, state string
}

// newMakeEnv builds the scratch directory. pathTools are extra executables to
// put on PATH (name to script).
func newMakeEnv(t *testing.T, pathTools map[string]string) *makeEnv {
	t.Helper()
	needShell(t)
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make on PATH")
	}

	work := filepath.Join(t.TempDir(), "check out")
	e := &makeEnv{
		t: t, makeBin: makeBin, work: work,
		bin: filepath.Join(work, "fake bin"), state: filepath.Join(work, "state"),
	}
	for _, name := range []string{"Makefile", filepath.Join("scripts", "retry.sh")} {
		raw, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(work, name), string(raw), 0o600)
	}
	writeFile(t, filepath.Join(e.bin, "go"), fakeGo, 0o700)
	for name, body := range pathTools {
		writeFile(t, filepath.Join(e.bin, name), body, 0o700)
	}
	for _, name := range []string{"sh", "cat", "mkdir", "chmod", "date", "sleep", "true", "awk", "grep", "mktemp", "rm", "timeout"} {
		found, err := exec.LookPath(name)
		if err != nil {
			if name == "timeout" {
				continue // optional: retry.sh degrades without it
			}
			t.Skipf("no %s on PATH", name)
		}
		if err := os.Symlink(found, filepath.Join(e.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(e.state, ".keep"), "", 0o600)
	return e
}

// set seeds the fake toolchain's state (see fakeGo).
func (e *makeEnv) set(name, content string) {
	e.t.Helper()
	writeFile(e.t, filepath.Join(e.state, name), content, 0o600)
}

// makeRun is the outcome of one `make target`. The logs are cumulative over
// every run in the same makeEnv.
type makeRun struct {
	code    int
	out     string
	goLog   []string // every `go ...` call, in order
	toolLog []string // every call of an installed (or PATH) tool, in order
}

// run runs `make target` with env added to a CLEAN environment -- not
// os.Environ(): under `make test` this process inherits MAKEFLAGS and any
// command-line variable overrides, and they must not leak into the make under
// test.
func (e *makeEnv) run(target string, env ...string) makeRun {
	e.t.Helper()
	cmd := exec.CommandContext(e.t.Context(), e.makeBin, target)
	cmd.Dir = e.work
	cmd.Env = append([]string{
		"PATH=" + e.bin,
		"HOME=" + e.work,
		"TMPDIR=" + e.state,
		"FAKE_DIR=" + e.state,
		"FAKE_DEFAULT_MSG=" + fetchFailure,
		"RETRY_DELAY=0",
		"RETRY_ATTEMPTS=4",
	}, env...)
	raw, err := cmd.CombinedOutput()
	return makeRun{
		code:    exitCode(e.t, err),
		out:     string(raw),
		goLog:   lines(e.t, filepath.Join(e.state, "go.log")),
		toolLog: lines(e.t, filepath.Join(e.state, "tool.log")),
	}
}

// runMake is one run in a fresh makeEnv seeded with files.
func runMake(t *testing.T, target string, files, pathTools map[string]string) makeRun {
	t.Helper()
	e := newMakeEnv(t, pathTools)
	for name, content := range files {
		e.set(name, content)
	}
	return e.run(target)
}

const (
	goList           = "go list -deps -test ./..."
	goInstallVuln    = "go install golang.org/x/vuln/cmd/govulncheck@"
	goInstallLint    = "go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"
	nonDownloadError = "go: cannot install cross-compiled binaries when GOBIN is set"
)

// The point of the whole change. Each case is a gate whose TOOL reports a
// problem; whatever the downloads did first, the verdict command runs exactly
// once and the gate fails.
func TestMakeNeverRetriesAVerdict(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		target    string
		files     map[string]string
		pathTools map[string]string
		verdict   string // the command whose failure is the verdict
		inGoLog   bool   // whether that command is a `go` call or a tool call
	}{
		"vulnerability found, after two failed tool downloads": {
			target:  "vulncheck",
			files:   map[string]string{"install.fails": "2", "tool.exit": "3"},
			verdict: "govulncheck ./...",
		},
		"lint finding, after a failed module download": {
			target:  "lint",
			files:   map[string]string{"list.fails": "1", "tool.exit": "1"},
			verdict: "golangci-lint run",
		},
		"lint finding from a golangci-lint on PATH": {
			target:    "lint",
			files:     map[string]string{"tool.exit": "1"},
			pathTools: map[string]string{"golangci-lint": fakeLint},
			verdict:   "golangci-lint run",
		},
		"go vet failure": {
			target:  "lint",
			files:   map[string]string{"vet.exit": "1"},
			verdict: "go vet ./...",
			inGoLog: true,
		},
		"test failure, after a failed module download": {
			target:  "test",
			files:   map[string]string{"list.fails": "1", "test.exit": "1"},
			verdict: "go test ./...",
			inGoLog: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := runMake(t, tc.target, tc.files, tc.pathTools)
			if r.code == 0 {
				t.Errorf("make %s passed; the verdict was lost\n%s", tc.target, r.out)
			}
			log := r.toolLog
			if tc.inGoLog {
				log = r.goLog
			}
			if n := count(log, tc.verdict); n != 1 {
				t.Errorf("%q ran %d times, want exactly 1\n%s", tc.verdict, n, r.out)
			}
		})
	}
}

// Every download a gate depends on is retried: the module fetch, and the
// install of each tool.
func TestMakeRetriesTheDownloads(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		target, install, verdict string
	}{
		"vulncheck": {"vulncheck", goInstallVuln, "govulncheck ./..."},
		"lint":      {"lint", goInstallLint, "golangci-lint run"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := runMake(t, tc.target, map[string]string{"list.fails": "1", "install.fails": "2"}, nil)
			if r.code != 0 {
				t.Fatalf("make %s failed (%d) though every download recovered\n%s", tc.target, r.code, r.out)
			}
			if n := count(r.goLog, goList); n != 2 {
				t.Errorf("module download ran %d times, want 2 (one failure, one success)\n%s", n, r.out)
			}
			if n := count(r.goLog, tc.install); n != 3 {
				t.Errorf("tool install ran %d times, want 3 (two failures, one success)\n%s", n, r.out)
			}
			if n := count(r.toolLog, tc.verdict); n != 1 {
				t.Errorf("%q ran %d times, want exactly 1\n%s", tc.verdict, n, r.out)
			}
		})
	}
}

// Every Go gate fetches the module graph first, through the retry: when that
// fetch never recovers the gate fails after the bounded attempts and its
// verdict command is never run on a half-prepared tree.
func TestMakeGatesFetchModulesFirst(t *testing.T) {
	t.Parallel()
	for target, verdict := range map[string]string{
		"test":      "go test ",
		"lint":      "go vet ",
		"vulncheck": "go install ", // not even the tool install is reached
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			r := runMake(t, target, map[string]string{"list.fails": "99"}, nil)
			if r.code == 0 {
				t.Fatalf("make %s passed without its modules\n%s", target, r.out)
			}
			if n := count(r.goLog, goList); n != 4 {
				t.Errorf("module download ran %d times, want exactly RETRY_ATTEMPTS=4\n%s", n, r.out)
			}
			if n := count(r.goLog, verdict); n != 0 || len(r.toolLog) != 0 {
				t.Errorf("%q ran %d times and tools ran %v, want nothing after a failed module download\n%s", verdict, n, r.toolLog, r.out)
			}
		})
	}
}

// A tool download that never recovers fails the gate after the bounded
// attempts, and the tool is never run.
func TestMakeFailsWhenTheDownloadNeverRecovers(t *testing.T) {
	t.Parallel()
	for target, install := range map[string]string{"vulncheck": goInstallVuln, "lint": goInstallLint} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			r := runMake(t, target, map[string]string{"install.fails": "99"}, nil)
			if r.code == 0 {
				t.Fatalf("make %s passed without its tool\n%s", target, r.out)
			}
			if n := count(r.goLog, install); n != 4 {
				t.Errorf("tool install ran %d times, want exactly RETRY_ATTEMPTS=4\n%s", n, r.out)
			}
			if len(r.toolLog) != 0 {
				t.Errorf("the tool ran although its install failed: %v", r.toolLog)
			}
		})
	}
}

// A failure of a download step that is not a download problem is reported at
// once, not after four attempts.
func TestMakeDoesNotRetryANonDownloadFailure(t *testing.T) {
	t.Parallel()
	for target, tc := range map[string]struct{ verb, call string }{
		"vulncheck": {"install", goInstallVuln},
		"test":      {"list", goList},
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			r := runMake(t, target, map[string]string{tc.verb + ".fails": "99", tc.verb + ".msg": nonDownloadError}, nil)
			if r.code == 0 {
				t.Fatalf("make %s passed\n%s", target, r.out)
			}
			if n := count(r.goLog, tc.call); n != 1 {
				t.Errorf("%q ran %d times, want exactly 1\n%s", tc.call, n, r.out)
			}
			if !strings.Contains(r.out, nonDownloadError) {
				t.Errorf("go's own message was lost:\n%s", r.out)
			}
		})
	}
}

// A golangci-lint on PATH is used as it is: nothing is installed.
func TestMakeLintPrefersPathBinary(t *testing.T) {
	t.Parallel()
	r := runMake(t, "lint", nil, map[string]string{"golangci-lint": fakeLint})
	if r.code != 0 {
		t.Fatalf("make lint failed (%d)\n%s", r.code, r.out)
	}
	if n := count(r.goLog, "go install "); n != 0 {
		t.Errorf("go install ran %d times, want 0 with golangci-lint on PATH\n%s", n, r.out)
	}
	if n := count(r.toolLog, "golangci-lint run"); n != 1 {
		t.Errorf("golangci-lint ran %d times, want 1\n%s", n, r.out)
	}
}

// The pinned tool already in bin/tools, built with this toolchain, is used
// without asking the proxy for anything: the gates work offline. A changed
// pin or a changed toolchain still reinstalls.
func TestMakeReinstallsAToolOnlyWhenItIsStale(t *testing.T) {
	t.Parallel()
	for target, tc := range map[string]struct{ install, verdict, pin string }{
		"vulncheck": {goInstallVuln, "govulncheck ./...", "GOVULNCHECK_VERSION"},
		"lint":      {goInstallLint, "golangci-lint run", "GOLANGCI_LINT_VERSION"},
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			e := newMakeEnv(t, nil)
			if r := e.run(target); r.code != 0 || count(r.goLog, tc.install) != 1 {
				t.Fatalf("first run: exit %d, %d installs, want 0 and 1\n%s", r.code, count(r.goLog, tc.install), r.out)
			}

			// The proxy goes away. Nothing needs it.
			e.set("install.fails", "99")
			r := e.run(target)
			if r.code != 0 {
				t.Fatalf("offline run with the tool present failed (%d)\n%s", r.code, r.out)
			}
			if n := count(r.goLog, tc.install); n != 1 {
				t.Errorf("go install ran %d times in all, want 1: the present tool must not be reinstalled\n%s", n, r.out)
			}
			if n := count(r.toolLog, tc.verdict); n != 2 {
				t.Errorf("%q ran %d times over two runs, want 2\n%s", tc.verdict, n, r.out)
			}

			// A different pin: the binary is stale, the install is attempted
			// (and, with the proxy still away, fails the gate).
			r = e.run(target, tc.pin+"=v9.9.9")
			if r.code == 0 || count(r.goLog, tc.install+"v9.9.9") != 4 {
				t.Errorf("changed pin: exit %d, %d installs of v9.9.9, want a failure after 4\n%s",
					r.code, count(r.goLog, tc.install+"v9.9.9"), r.out)
			}
			if n := count(r.toolLog, tc.verdict); n != 2 {
				t.Errorf("the stale tool was run (%d runs in all, want still 2)\n%s", n, r.out)
			}

			// The proxy is back. The stale tool was removed when its reinstall
			// was attempted, so this run installs the pinned version again...
			e.set("install.fails", "0")
			before := count(r.goLog, tc.install)
			r = e.run(target)
			if r.code != 0 || count(r.goLog, tc.install) != before+1 {
				t.Fatalf("proxy back: exit %d, %d new installs, want 0 and 1\n%s",
					r.code, count(r.goLog, tc.install)-before, r.out)
			}
			// ...and with that in place and current, a changed toolchain alone
			// makes it stale.
			e.set("goversion", "go1.99.0")
			before = count(r.goLog, tc.install)
			r = e.run(target)
			if r.code != 0 || count(r.goLog, tc.install) != before+1 {
				t.Errorf("changed toolchain: exit %d, %d new installs, want 0 and 1\n%s",
					r.code, count(r.goLog, tc.install)-before, r.out)
			}
		})
	}
}

// A tool that is in bin/tools at the right version but cannot be run here --
// built for another platform, stripped of its exec bit, truncated -- is not
// "already installed". It is removed and installed afresh, and the gate then
// runs the new one.
func TestMakeReinstallsAToolItCannotRun(t *testing.T) {
	t.Parallel()
	for name, damage := range map[string]func(t *testing.T, e *makeEnv, tool string){
		"built for another architecture": func(_ *testing.T, e *makeEnv, _ string) { e.set("goarch", "arm64") },
		"built for another OS":           func(_ *testing.T, e *makeEnv, _ string) { e.set("goos", "darwin") },
		"no exec bit": func(t *testing.T, _ *makeEnv, tool string) {
			if err := os.Chmod(tool, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"truncated": func(t *testing.T, _ *makeEnv, tool string) {
			writeFile(t, tool, "\x7fELF", 0o700)
			if err := os.Remove(tool + ".meta"); err != nil { // go version -m cannot read it either
				t.Fatal(err)
			}
		},
	} {
		for target, tc := range map[string]struct{ install, verdict, tool string }{
			"vulncheck": {goInstallVuln, "govulncheck ./...", "govulncheck"},
			"lint":      {goInstallLint, "golangci-lint run", "golangci-lint"},
		} {
			t.Run(name+"/"+target, func(t *testing.T) {
				t.Parallel()
				e := newMakeEnv(t, nil)
				if r := e.run(target); r.code != 0 || count(r.goLog, tc.install) != 1 {
					t.Fatalf("first run: exit %d, %d installs, want 0 and 1\n%s", r.code, count(r.goLog, tc.install), r.out)
				}
				damage(t, e, filepath.Join(e.work, "bin", "tools", tc.tool))
				r := e.run(target)
				if r.code != 0 {
					t.Fatalf("the gate did not recover from a tool it cannot run (exit %d)\n%s", r.code, r.out)
				}
				if n := count(r.goLog, tc.install); n != 2 {
					t.Errorf("go install ran %d times in all, want 2: the unusable tool must be reinstalled\n%s", n, r.out)
				}
				if n := count(r.toolLog, tc.verdict); n != 2 {
					t.Errorf("%q ran %d times over two runs, want 2: the reinstalled tool must be the one that runs\n%s", tc.verdict, n, r.out)
				}
			})
		}
	}
}

// Every gate works from a checkout whose path has a space in it. (Every other
// make test here runs in such a directory too; this one says so.)
func TestMakeWorksFromAPathWithASpace(t *testing.T) {
	t.Parallel()
	for target, verdict := range map[string]string{"vulncheck": "govulncheck ./...", "lint": "golangci-lint run"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			e := newMakeEnv(t, nil)
			if !strings.Contains(e.work, " ") {
				t.Fatalf("the scratch checkout %q has no space in its path", e.work)
			}
			r := e.run(target)
			if r.code != 0 || count(r.toolLog, verdict) != 1 {
				t.Errorf("exit %d, %q ran %d times; want 0 and 1\n%s", r.code, verdict, count(r.toolLog, verdict), r.out)
			}
		})
	}
}

// The rule the tests above enforce for today's gates, as a rule about the
// Makefile's text, so that a NEW recipe cannot put a verdict behind the retry
// either: the only things $(RETRY) may run are `go list` and `go install`.
func TestMakefileRetriesOnlyDownloads(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := regexp.MustCompile(`\$\(RETRY\) go (list|install) `)
	uses := 0
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "RETRY ") {
			continue
		}
		if !strings.Contains(line, "$(RETRY)") && !strings.Contains(line, "retry.sh") {
			continue
		}
		uses++
		if strings.Count(line, "$(RETRY)") != 1 || !allowed.MatchString(line) {
			t.Errorf("Makefile:%d puts something other than a download behind the retry:\n%s", i+1, line)
		}
	}
	if uses != 2 {
		t.Errorf("found %d uses of $(RETRY) in the Makefile, want 2 (the module fetch and the tool install); update this test with the Makefile", uses)
	}
}
