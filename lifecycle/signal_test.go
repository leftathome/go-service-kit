//go:build unix

package lifecycle_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/leftathome/go-service-kit/lifecycle"
)

// Everything else in this package is tested inside a testing/synctest bubble,
// which cannot receive signals: delivery comes from a runtime goroutine
// outside the bubble. So this one test uses a REAL SIGTERM, a real 127.0.0.1
// listener, and real (short) durations, to prove that the sequence the fake
// clock verifies is actually reached by the signal path.
//
// It is not parallel and it must not be: it raises a process-wide signal. The
// test waits until the server answers before signalling, which guarantees
// Run's signal.Notify is already installed -- SIGTERM's default disposition
// would otherwise kill the test binary.
func TestRealSIGTERMDrivesTheSequence(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ready := httpapi.NewReadiness()
	admin := httpapi.NewAdmin(httpapi.AdminOptions{Readiness: ready})

	mux := http.NewServeMux()
	mux.Handle("GET /readyz", admin.Handler())
	mux.HandleFunc("GET /fast", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	})

	const propagation = 750 * time.Millisecond
	flushed := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- lifecycle.Run(context.Background(), lifecycle.Spec{
			Servers: []*http.Server{{
				Handler:           mux,
				ReadHeaderTimeout: time.Second,
			}},
			Listeners:        []net.Listener{ln},
			Readiness:        ready,
			PropagationDelay: propagation,
			DrainTimeout:     5 * time.Second,
			Logger:           slog.New(slog.DiscardHandler),
			Flush: func(context.Context) error {
				close(flushed)
				return nil
			},
		})
	}()

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	get := func(path string) int {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			return 0
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Wait until it is actually serving. Reaching this point also means
	// signal.Notify has run, because Run installs it before it listens.
	deadline := time.Now().Add(5 * time.Second)
	for get("/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("raise SIGTERM: %v", err)
	}

	// 1. Readiness flips promptly. Poll rather than assert instantly: the
	//    signal crosses a runtime goroutine, so "immediately" here means
	//    "without waiting for the propagation delay", which the timing check
	//    below is what actually pins down.
	flipDeadline := time.Now().Add(propagation / 2)
	for !ready.ShuttingDown() {
		if time.Now().After(flipDeadline) {
			t.Fatal("readiness did not flip within half the propagation delay")
		}
		time.Sleep(time.Millisecond)
	}
	if code := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz after SIGTERM = %d, want 503", code)
	}

	// 2. Still serving inside the propagation window.
	if code := get("/fast"); code != http.StatusOK {
		t.Errorf("request during the propagation window = %d, want 200", code)
	}

	// 3 through 5.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}

	select {
	case <-flushed:
	default:
		t.Error("Flush never ran")
	}

	if code := get("/fast"); code == http.StatusOK {
		t.Error("server still answering after Run returned")
	}
}
