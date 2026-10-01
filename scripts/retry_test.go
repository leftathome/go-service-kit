// Package scripts holds the tests for the shell helpers in this directory and
// for the Makefile wiring that uses them. There is no Go code here to import.
//
// Two things are under test (quark-q5a):
//
//   - retry.sh itself: a download that fails and then succeeds is a success, one
//     that never succeeds fails with its own exit status after a bounded number
//     of attempts, and the time limits hold.
//   - the Makefile's use of it: the DOWNLOAD of a tool is retried, the tool's
//     VERDICT never is. Those tests run the real Makefile against a fake `go`
//     that fails on demand and installs a fake tool that records every call.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// flaky is a command that fails with status $2 until it has been run more
// than $1 times, counting its runs in ./runs. A third argument of "hang" makes
// a failing run sleep instead of exiting.
const flaky = `#!/bin/sh
echo run >>runs
n=$(wc -l <runs)
[ "$n" -gt "$1" ] && exit 0
[ "${3:-}" = hang ] && exec sleep 60
echo "flaky: net/http: TLS handshake timeout" >&2
exit "$2"
`

// fakeSleep stands in for sleep(1) so the backoff can be read without being
// waited for.
const fakeSleep = `#!/bin/sh
echo "$1" >>"$SLEEP_LOG"
`

// fakeGo stands in for the go command under make. Every call is appended to
// $FAKE_DIR/go.log. `go <verb>` fails while $FAKE_DIR/<verb>.fails holds a
// number above zero (counting it down), then exits with $FAKE_DIR/<verb>.exit
// (default 0). A successful `go install` writes a fake tool into $GOBIN that
// logs its own calls to $FAKE_DIR/tool.log and exits with $FAKE_DIR/tool.exit.
const fakeGo = `#!/bin/sh
echo "go $*" >>"$FAKE_DIR/go.log"
verb=$1
left=$(cat "$FAKE_DIR/$verb.fails" 2>/dev/null || echo 0)
if [ "$left" -gt 0 ]; then
	echo $((left - 1)) >"$FAKE_DIR/$verb.fails"
	echo "fake go $verb: net/http: TLS handshake timeout" >&2
	exit 1
fi
if [ "$verb" = install ]; then
	name=${2##*/}
	name=${name%%@*}
	mkdir -p "$GOBIN"
	cat >"$GOBIN/$name" <<TOOL
#!/bin/sh
echo "$name \$*" >>"$FAKE_DIR/tool.log"
exit \$(cat "$FAKE_DIR/tool.exit" 2>/dev/null || echo 0)
TOOL
	chmod +x "$GOBIN/$name"
fi
exit "$(cat "$FAKE_DIR/$verb.exit" 2>/dev/null || echo 0)"
`

// fakeLint is a golangci-lint found on PATH: it logs like an installed tool.
const fakeLint = `#!/bin/sh
echo "golangci-lint $*" >>"$FAKE_DIR/tool.log"
exit "$(cat "$FAKE_DIR/tool.exit" 2>/dev/null || echo 0)"
`

func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("retry.sh is a POSIX shell script")
	}
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
	if err := os.WriteFile(path, []byte(content), mode); err != nil { //nolint:gosec // G306: test fixtures under t.TempDir, some executable
		t.Fatal(err)
	}
}

// lines returns the non-empty lines of a log file, or nil if it is absent.
func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: under t.TempDir
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

// runRetry runs retry.sh in a fresh directory holding the flaky command, with
// env added to the environment, and returns its exit status, its combined
// output and that directory.
func runRetry(t *testing.T, env []string, args ...string) (code int, out, dir string) {
	t.Helper()
	needShell(t)
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "flaky"), flaky, 0o700)
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{retryScript(t)}, args...)...) //nolint:gosec // G204: fixed script, test-chosen arguments
	cmd.Dir = dir
	// Blank any RETRY_* setting this process inherited, so only env decides.
	cmd.Env = append(os.Environ(), "RETRY_ATTEMPTS=", "RETRY_DELAY=", "RETRY_ATTEMPT_TIMEOUT=", "RETRY_BUDGET=")
	cmd.Env = append(cmd.Env, env...)
	raw, err := cmd.CombinedOutput()
	return exitCode(t, err), string(raw), dir
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

func TestRetryDoesNotRetryAMissingCommand(t *testing.T) {
	t.Parallel()
	code, out, _ := runRetry(t, []string{"RETRY_DELAY=0"}, "./no-such-command")
	if code != 127 {
		t.Errorf("exit %d, want 127\n%s", code, out)
	}
	if strings.Contains(out, "retrying in") || !strings.Contains(out, "retry: not retrying") {
		t.Errorf("a missing command must fail at once, got:\n%s", out)
	}
}

func TestRetryKillsAHungAttempt(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout(1) on PATH; retry.sh cannot bound a hung attempt here")
	}
	start := time.Now()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=0", "RETRY_ATTEMPT_TIMEOUT=1"}, "./flaky", "1", "1", "hang")
	if code != 0 || runs(t, dir) != 2 {
		t.Fatalf("exit %d after %d runs, want 0 after 2\n%s", code, runs(t, dir), out)
	}
	if !strings.Contains(out, "attempt 1/4 failed (killed after 1s)") {
		t.Errorf("the hung attempt was not reported as killed:\n%s", out)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("took %s; the 60s hang was not cut short", d)
	}
}

// The budget bounds the whole thing: a wait that would overrun it is not
// started, and the status is still the command's own.
func TestRetryStopsAtTheBudget(t *testing.T) {
	t.Parallel()
	code, out, dir := runRetry(t, []string{"RETRY_DELAY=5", "RETRY_BUDGET=2"}, "./flaky", "99", "9")
	if code != 9 || runs(t, dir) != 1 {
		t.Fatalf("exit %d after %d runs, want 9 after 1\n%s", code, runs(t, dir), out)
	}
	if !strings.Contains(out, "would overrun the 2s budget") {
		t.Errorf("missing the budget line in:\n%s", out)
	}
}

func TestRetryRejectsBadUsage(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env  []string
		args []string
	}{
		"no command":            {nil, nil},
		"attempts not a number": {[]string{"RETRY_ATTEMPTS=many"}, []string{"./flaky", "0", "1"}},
		"zero attempts":         {[]string{"RETRY_ATTEMPTS=0"}, []string{"./flaky", "0", "1"}},
		"negative delay":        {[]string{"RETRY_DELAY=-1"}, []string{"./flaky", "0", "1"}},
		"zero budget":           {[]string{"RETRY_BUDGET=0"}, []string{"./flaky", "0", "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out, dir := runRetry(t, tc.env, tc.args...)
			if code != 2 || runs(t, dir) != 0 {
				t.Errorf("exit %d after %d runs, want 2 after 0\n%s", code, runs(t, dir), out)
			}
		})
	}
}

func TestRetryPassesArgumentsThrough(t *testing.T) {
	t.Parallel()
	code, out, _ := runRetry(t, nil, "printf", "[%s]", "a b", "", "c")
	if code != 0 || out != "[a b][][c]" {
		t.Errorf("exit %d, output %q; want 0 and \"[a b][][c]\"", code, out)
	}
}

// makeRun is one run of a real Makefile target against the fake toolchain.
type makeRun struct {
	code    int
	out     string
	goLog   []string // every `go ...` call, in order
	toolLog []string // every call of an installed (or PATH) tool, in order
}

// runMake copies the Makefile and retry.sh into a scratch directory and runs
// `make target` there with a PATH that holds ONLY the fake go and the handful
// of system commands the recipes use -- so neither the real toolchain nor a
// golangci-lint installed on this machine can take part. files seeds the fake
// toolchain's state (see fakeGo).
func runMake(t *testing.T, target string, files map[string]string, pathTools map[string]string) makeRun {
	t.Helper()
	needShell(t)
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make on PATH")
	}

	work := t.TempDir()
	for _, name := range []string{"Makefile", filepath.Join("scripts", "retry.sh")} {
		raw, err := os.ReadFile(filepath.Join("..", name)) //nolint:gosec // G304: this repository's own files
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(work, name), string(raw), 0o600)
	}

	bin := filepath.Join(work, "fakebin")
	writeFile(t, filepath.Join(bin, "go"), fakeGo, 0o700)
	for name, body := range pathTools {
		writeFile(t, filepath.Join(bin, name), body, 0o700)
	}
	for _, name := range []string{"sh", "cat", "mkdir", "chmod", "date", "sleep", "true", "timeout"} {
		found, err := exec.LookPath(name)
		if err != nil {
			if name == "timeout" {
				continue // optional: retry.sh degrades without it
			}
			t.Skipf("no %s on PATH", name)
		}
		if err := os.Symlink(found, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}

	state := filepath.Join(work, "state")
	writeFile(t, filepath.Join(state, ".keep"), "", 0o600)
	for name, content := range files {
		writeFile(t, filepath.Join(state, name), content, 0o600)
	}

	cmd := exec.CommandContext(t.Context(), makeBin, target) //nolint:gosec // G204: make from PATH, a fixed target name
	cmd.Dir = work
	// A clean environment, not os.Environ(): under `make test` this process
	// inherits MAKEFLAGS and any command-line variable overrides, and they
	// must not leak into the make under test.
	cmd.Env = []string{
		"PATH=" + bin,
		"HOME=" + work,
		"FAKE_DIR=" + state,
		"RETRY_DELAY=0",
		"RETRY_ATTEMPTS=4",
	}
	raw, err := cmd.CombinedOutput()
	return makeRun{
		code:    exitCode(t, err),
		out:     string(raw),
		goLog:   lines(t, filepath.Join(state, "go.log")),
		toolLog: lines(t, filepath.Join(state, "tool.log")),
	}
}

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

func TestMakeRetriesTheDownloads(t *testing.T) {
	t.Parallel()
	r := runMake(t, "vulncheck", map[string]string{"list.fails": "1", "install.fails": "2"}, nil)
	if r.code != 0 {
		t.Fatalf("make vulncheck failed (%d) though every download recovered\n%s", r.code, r.out)
	}
	if n := count(r.goLog, "go list -deps -test ./..."); n != 2 {
		t.Errorf("module download ran %d times, want 2 (one failure, one success)\n%s", n, r.out)
	}
	if n := count(r.goLog, "go install golang.org/x/vuln/cmd/govulncheck@"); n != 3 {
		t.Errorf("tool install ran %d times, want 3 (two failures, one success)\n%s", n, r.out)
	}
	if n := count(r.toolLog, "govulncheck ./..."); n != 1 {
		t.Errorf("govulncheck ran %d times, want exactly 1\n%s", n, r.out)
	}
}

// A download that never recovers fails the gate after the bounded attempts,
// and the tool is never run on a half-prepared tree.
func TestMakeFailsWhenTheDownloadNeverRecovers(t *testing.T) {
	t.Parallel()
	r := runMake(t, "vulncheck", map[string]string{"install.fails": "99"}, nil)
	if r.code == 0 {
		t.Fatalf("make vulncheck passed without its tool\n%s", r.out)
	}
	if n := count(r.goLog, "go install "); n != 4 {
		t.Errorf("tool install ran %d times, want exactly RETRY_ATTEMPTS=4\n%s", n, r.out)
	}
	if len(r.toolLog) != 0 {
		t.Errorf("the tool ran although its install failed: %v", r.toolLog)
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
