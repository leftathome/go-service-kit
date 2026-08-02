package obs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LoggerOptions configures NewLogger. The zero value produces a JSON logger at
// info level on stdout with DefaultRedactKeys applied.
type LoggerOptions struct {
	// Level is the minimum level emitted. The zero value is slog.LevelInfo.
	Level slog.Level
	// Output defaults to os.Stdout, which is what promtail scrapes into Loki.
	Output io.Writer
	// ServiceName and ServiceVersion are attached to every record so one Loki
	// stream can be split by service without inventing a label per service.
	ServiceName    string
	ServiceVersion string
	// RedactKeys are attribute keys whose values are masked, matched
	// case-insensitively. Nil means DefaultRedactKeys; an explicitly empty,
	// non-nil slice disables redaction (do not do that in a service).
	RedactKeys []string
	// AddSource includes file:line. Off by default: it is not free, and the
	// call site is rarely the thing you need in a request log.
	AddSource bool
}

// The deny list is published in three parts so a service can compose the
// policy it actually needs instead of choosing between "all of it" and "none of
// it". [DefaultRedactKeys] is the three concatenated, and remains what
// LoggerOptions.RedactKeys nil means.
//
// Treat all four as READ-ONLY. They are slices, so a caller could append into
// spare capacity and change what every other logger in the process redacts;
// build a new slice instead ([RedactKeysExcept], slices.Concat, append to a
// clone).
var (
	// CredentialRedactKeys are keys whose values are secrets or session
	// material. These are not tunable per service: a credential in Loki is an
	// incident in every domain, so [RedactKeysExcept] refuses to remove them.
	CredentialRedactKeys = []string{
		"authorization",
		"proxy-authorization",
		"cookie",
		"set-cookie",
		"credential",
		"password",
		"passwd",
		"secret",
		"client_secret",
		"token",
		"access_token",
		"refresh_token",
		"id_token",
		"api_key",
		"apikey",
		"private_key",
		"signature",
		"session",
		"session_id",
	}

	// PIIRedactKeys are keys that carry personal data about a human. Genuine
	// PII, not domain vocabulary that happens to describe people.
	PIIRedactKeys = []string{
		"email",
		"phone",
		"address",
		"postal_code",
		"ssn",
		"date_of_birth",
	}

	// RequestMaterialRedactKeys are keys that echo a request back verbatim. A
	// URL query STRING is included because credentials routinely ride in one;
	// the word "query" on its own is not, because in a search service it is
	// the subject of every useful log line.
	RequestMaterialRedactKeys = []string{
		"body",
		"request_body",
		"response_body",
		"payload",
		"query_string",
		"url.query",
		"raw_url",
		"raw_query",
	}

	// DefaultRedactKeys is the deny list applied when LoggerOptions.RedactKeys
	// is nil: credentials, PII, and raw request material.
	//
	// Extend it rather than replacing it:
	//
	//	RedactKeys: append(slices.Clone(obs.DefaultRedactKeys), "serial_number")
	//
	// To take something OUT, use [RedactKeysExcept] -- and read its doc first,
	// because doing so is a deliberate act with a reason worth writing down.
	DefaultRedactKeys = slices.Concat(CredentialRedactKeys, PIIRedactKeys, RequestMaterialRedactKeys)
)

// RedactKeysExcept returns a fresh copy of [DefaultRedactKeys] with the named
// keys removed, matched case-insensitively. Names that are not in the list are
// ignored, so the call does not have to track the kit's exact contents.
//
// CHANGING THE DENY LIST IS A DELIBERATE ACT. The list is a fleet default, and
// the default is chosen to be wrong in the safe direction. Removing a key means
// asserting that in THIS service, under THIS key, the value is operator-authored
// configuration or a bounded identifier -- not user data, not third-party
// content, not a credential. Write the reason at the call site; the next person
// to read it will be trying to work out whether it is still true.
//
// nagus is the worked example and the reason this function exists. It is an
// acquisition service: "query" is the operator's own eBay search string and
// "lat"/"lon" are the whole subject of its land category, so redacting them
// turned every useful debug line into a hash. Its previous options were to
// replace the list wholesale (and silently lose a credential key on the next
// kit upgrade) or to disable redaction entirely.
//
//	// A source's query and a parcel's coordinates are operator-authored
//	// configuration in this service, not user data. Listing free text stays
//	// redacted and never appears above debug level.
//	RedactKeys: obs.RedactKeysExcept("query", "search", "lat", "lon")
//
// It PANICS if asked to remove a key from [CredentialRedactKeys]. That is a
// programming error, it is detectable only at the moment of the call, and a
// process that starts anyway would log credentials for as long as it ran. A
// service with a genuine need for a narrower list can still build one from the
// three exported components; the panic guards the convenience path, it is not
// a wall.
func RedactKeysExcept(keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if slices.Contains(CredentialRedactKeys, k) {
			panic("obs: RedactKeysExcept cannot remove the credential key " + strconv.Quote(k) +
				"; credential redaction is not service-tunable")
		}
		drop[k] = struct{}{}
	}
	out := make([]string, 0, len(DefaultRedactKeys))
	for _, k := range DefaultRedactKeys {
		if _, bad := drop[strings.ToLower(k)]; !bad {
			out = append(out, k)
		}
	}
	return out
}

const redactedPrefix = "[redacted:"

// Redact masks a value for logging. It returns a stable, non-reversible token
// of the form "[redacted:<8 hex>]", so two log lines carrying the same value
// can still be correlated without the value itself ever reaching Loki. The
// empty string maps to the empty string: "this field was absent" is not
// sensitive, and a token there is only noise.
func Redact(v string) string {
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return redactedPrefix + hex.EncodeToString(sum[:4]) + "]"
}

// RedactAttr builds a masked slog.Attr. Use it at the call site when a value is
// sensitive but its key is not on the deny list.
func RedactAttr(key, value string) slog.Attr {
	return slog.String(key, Redact(value))
}

// ParseLevel maps a config string to a slog.Level. The empty string is info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("obs: unknown log level %q (want debug, info, warn, or error)", s)
	}
}

// NewLogger builds the house logger: JSON to stdout, trace-correlated, with
// deny-list redaction applied before anything is serialized.
func NewLogger(opts LoggerOptions) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	keys := opts.RedactKeys
	if keys == nil {
		keys = DefaultRedactKeys
	}
	deny := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		deny[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
	}

	base := slog.Handler(slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:       opts.Level,
		AddSource:   opts.AddSource,
		ReplaceAttr: redactingReplacer(deny),
	}))

	logger := slog.New(newTraceHandler(base))
	if opts.ServiceName != "" {
		logger = logger.With(slog.String("service", opts.ServiceName))
	}
	if opts.ServiceVersion != "" {
		logger = logger.With(slog.String("version", opts.ServiceVersion))
	}
	return logger
}

// redactingReplacer masks denied keys wherever they appear, including inside
// groups and inside attributes added through Logger.With. Doing it in
// ReplaceAttr rather than at the call site means a careless caller cannot
// bypass it.
func redactingReplacer(deny map[string]struct{}) func([]string, slog.Attr) slog.Attr {
	if len(deny) == 0 {
		return nil
	}
	return func(_ []string, a slog.Attr) slog.Attr {
		if _, bad := deny[strings.ToLower(a.Key)]; !bad {
			return a
		}
		// Resolve first: a LogValuer could otherwise hide a sensitive string.
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			// ReplaceAttr is not invoked for group values themselves, but be
			// defensive: collapse the whole group rather than walk into it.
			return slog.String(a.Key, redactedPrefix+"group]")
		}
		return slog.String(a.Key, Redact(v.String()))
	}
}

// handlerOp is one deferred WithAttrs or WithGroup call.
type handlerOp struct {
	group string
	attrs []slog.Attr
}

// traceHandler injects trace_id and span_id from the context when the context
// carries a valid span context, and adds nothing when it does not.
//
// The two fields are always emitted at the TOP LEVEL of the JSON object, never
// nested inside a group the caller happened to open. Grafana's Loki-to-Tempo
// correlation is configured with one fixed field path; if the path moved
// whenever a caller used WithGroup, the trace link would silently stop working
// for exactly the code that logs most carefully. To achieve that, groups are
// recorded and replayed rather than applied eagerly -- but only on the slow
// path, when a group is actually open AND a span is active.
type traceHandler struct {
	root     slog.Handler // no attrs or groups applied
	cur      slog.Handler // root with ops applied
	ops      []handlerOp
	hasGroup bool
}

func newTraceHandler(base slog.Handler) *traceHandler {
	return &traceHandler{root: base, cur: base}
}

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.cur.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.cur.Handle(ctx, r)
	}
	traceAttrs := []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
	if !h.hasGroup {
		// Fast path: no group is open, so record attributes land at the top
		// level already.
		r.AddAttrs(traceAttrs...)
		return h.cur.Handle(ctx, r)
	}
	// Slow path: re-apply the correlation fields to the ungrouped root, then
	// replay the caller's attrs and groups on top.
	target := h.root.WithAttrs(traceAttrs)
	for _, op := range h.ops {
		if op.group != "" {
			target = target.WithGroup(op.group)
			continue
		}
		target = target.WithAttrs(op.attrs)
	}
	return target.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &traceHandler{
		root:     h.root,
		cur:      h.cur.WithAttrs(attrs),
		ops:      append(slices.Clip(h.ops), handlerOp{attrs: attrs}),
		hasGroup: h.hasGroup,
	}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &traceHandler{
		root:     h.root,
		cur:      h.cur.WithGroup(name),
		ops:      append(slices.Clip(h.ops), handlerOp{group: name}),
		hasGroup: true,
	}
}
