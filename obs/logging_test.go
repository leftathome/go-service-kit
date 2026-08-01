package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// syncBuffer is a race-safe io.Writer for capturing log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines parses each JSON log line the handler wrote.
func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

func testLogger(buf *syncBuffer, redact ...string) *slog.Logger {
	return NewLogger(LoggerOptions{
		Level:          slog.LevelDebug,
		Output:         buf,
		ServiceName:    "kit-test",
		ServiceVersion: "0.0.1-test",
		RedactKeys:     redact,
	})
}

// spanCtx returns a context carrying a valid, sampled span context with known
// ids, so the assertion does not depend on a live SDK.
func spanCtx(t *testing.T) (context.Context, string, string) {
	t.Helper()
	tid, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	sid, err := trace.SpanIDFromHex("1112131415161718")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), tid.String(), sid.String()
}

func TestLoggerEmitsJSONWithServiceFields(t *testing.T) {
	buf := newSyncBuffer()
	testLogger(buf).Info("hello")

	lines := buf.lines(t)
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want 1", len(lines))
	}
	rec := lines[0]
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["service"] != "kit-test" {
		t.Errorf("service = %v, want kit-test", rec["service"])
	}
	if rec["version"] != "0.0.1-test" {
		t.Errorf("version = %v, want 0.0.1-test", rec["version"])
	}
}

func TestLoggerInjectsTraceCorrelation(t *testing.T) {
	buf := newSyncBuffer()
	ctx, wantTrace, wantSpan := spanCtx(t)
	testLogger(buf).InfoContext(ctx, "with span")

	rec := buf.lines(t)[0]
	if rec["trace_id"] != wantTrace {
		t.Errorf("trace_id = %v, want %v", rec["trace_id"], wantTrace)
	}
	if rec["span_id"] != wantSpan {
		t.Errorf("span_id = %v, want %v", rec["span_id"], wantSpan)
	}
}

func TestLoggerOmitsTraceCorrelationWithoutSpan(t *testing.T) {
	buf := newSyncBuffer()
	testLogger(buf).Info("no span")

	rec := buf.lines(t)[0]
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id present with no active span: %v", rec)
	}
	if _, ok := rec["span_id"]; ok {
		t.Errorf("span_id present with no active span: %v", rec)
	}
}

// An invalid (all-zero) span context must be treated as absent, not logged as
// a string of zeros that looks like a real id in Loki.
func TestLoggerOmitsInvalidSpanContext(t *testing.T) {
	buf := newSyncBuffer()
	ctx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	testLogger(buf).InfoContext(ctx, "invalid sc")

	rec := buf.lines(t)[0]
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id present for invalid span context: %v", rec)
	}
}

func TestLoggerCorrelationSurvivesWithAttrsAndGroups(t *testing.T) {
	buf := newSyncBuffer()
	ctx, wantTrace, _ := spanCtx(t)
	testLogger(buf).With("component", "store").WithGroup("req").InfoContext(ctx, "grouped")

	rec := buf.lines(t)[0]
	if rec["trace_id"] != wantTrace {
		t.Errorf("trace_id = %v, want %v (lost through With/WithGroup)", rec["trace_id"], wantTrace)
	}
	if rec["component"] != "store" {
		t.Errorf("component = %v, want store", rec["component"])
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	buf := newSyncBuffer()
	NewLogger(LoggerOptions{Level: slog.LevelInfo, Output: buf}).Debug("quiet")
	if s := buf.String(); s != "" {
		t.Errorf("debug record emitted at info level: %q", s)
	}
}

func TestRedactMasksValue(t *testing.T) {
	const secret = "123 Maple Street, Springfield"

	got := Redact(secret)
	if strings.Contains(got, "Maple") || strings.Contains(got, secret) {
		t.Fatalf("Redact leaked plaintext: %q", got)
	}
	if got == "" {
		t.Fatal("Redact returned empty for a non-empty value")
	}
	if !strings.HasPrefix(got, "[redacted:") || !strings.HasSuffix(got, "]") {
		t.Errorf("Redact = %q, want the [redacted:...] form", got)
	}
	if Redact(secret) != got {
		t.Error("Redact is not deterministic; correlation across log lines is impossible")
	}
	if Redact("a different value") == got {
		t.Error("Redact collapses distinct values to the same token")
	}
	if Redact("") != "" {
		t.Errorf("Redact(\"\") = %q, want empty", Redact(""))
	}
}

func TestRedactAttrMasksValue(t *testing.T) {
	a := RedactAttr("address", "123 Maple Street")
	if strings.Contains(a.Value.String(), "Maple") {
		t.Errorf("RedactAttr leaked plaintext: %v", a)
	}
	if a.Key != "address" {
		t.Errorf("RedactAttr key = %q, want address", a.Key)
	}
}

func TestLoggerRedactsDefaultSensitiveKeys(t *testing.T) {
	buf := newSyncBuffer()
	// nil RedactKeys means DefaultRedactKeys, so the safe path is the default.
	NewLogger(LoggerOptions{Level: slog.LevelDebug, Output: buf}).Info("req",
		"authorization", "Bearer sekrit-token",
		"query", "q=vintage+turntable",
		"password", "hunter2",
		"route", "/widgets/{id}",
	)

	out := buf.String()
	for _, leaked := range []string{"sekrit-token", "vintage", "hunter2"} {
		if strings.Contains(out, leaked) {
			t.Errorf("sensitive value %q reached the log output: %s", leaked, out)
		}
	}
	rec := buf.lines(t)[0]
	if rec["route"] != "/widgets/{id}" {
		t.Errorf("route = %v, want the untouched route template", rec["route"])
	}
	if s, _ := rec["password"].(string); !strings.HasPrefix(s, "[redacted:") {
		t.Errorf("password = %v, want a redaction token", rec["password"])
	}
}

func TestLoggerRedactsConfiguredKeysCaseInsensitively(t *testing.T) {
	buf := newSyncBuffer()
	testLogger(buf, "OwnerEmail").Info("item", "owneremail", "someone@example.com", "sku", "ABC-1")

	out := buf.String()
	if strings.Contains(out, "someone@example.com") {
		t.Errorf("configured key not redacted: %s", out)
	}
	if !strings.Contains(out, "ABC-1") {
		t.Errorf("non-sensitive field was redacted: %s", out)
	}
}

func TestLoggerRedactsNonStringAndGroupedValues(t *testing.T) {
	buf := newSyncBuffer()
	testLogger(buf).Info("payload",
		slog.Int("password", 1234),
		slog.Group("http", slog.String("body", "serial=deadbeef")),
	)

	out := buf.String()
	if strings.Contains(out, "1234") {
		t.Errorf("non-string sensitive value leaked: %s", out)
	}
	if strings.Contains(out, "deadbeef") {
		t.Errorf("sensitive value inside a group leaked: %s", out)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":      slog.LevelInfo,
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("ParseLevel(\"loud\") = nil error, want error")
	}
}
