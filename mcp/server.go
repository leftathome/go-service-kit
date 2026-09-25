package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
)

// DefaultProtocolVersion is the MCP protocol version initialize answers with
// when the client asks for none, or for one this package does not support.
const DefaultProtocolVersion = "2025-06-18"

// supportedProtocolVersions is newest first. It is deliberately ONE version:
// structuredContent first appears in 2025-06-18, and every value this package
// returns lives there. Agreeing to 2025-03-26 or 2024-11-05 would hand an older
// client a result whose only content it understands is a count -- a silent
// "no data". An older client is instead offered 2025-06-18 and, per the MCP
// lifecycle, disconnects if it cannot speak it.
var supportedProtocolVersions = []string{DefaultProtocolVersion}

// SupportedProtocolVersions returns the protocol versions initialize will
// agree to, newest first.
func SupportedProtocolVersions() []string { return slices.Clone(supportedProtocolVersions) }

// DefaultMaxBodyBytes bounds a request body when [Options.MaxBodyBytes] is
// zero. Tool arguments are a handful of small fields.
const DefaultMaxBodyBytes int64 = 1 << 20

// JSON-RPC 2.0 error codes used by this package.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Options configures a [Server].
type Options struct {
	// Name is serverInfo.name in the initialize result. Required.
	Name string

	// Version is serverInfo.version. Empty means "0.0.0".
	Version string

	// Instructions, when set, is returned as the initialize result's
	// instructions: server-wide guidance for the agent, such as how untrusted
	// values are marked. It is fixed at construction.
	Instructions string

	// Logger receives the detail of every internal error and handler panic.
	// Nil means slog.Default().
	Logger *slog.Logger

	// MaxBodyBytes caps the request body. Zero means [DefaultMaxBodyBytes];
	// negative is refused by [New].
	MaxBodyBytes int64

	// InternalErrorMessage is the JSON-RPC -32603 message every internal
	// failure is answered with, e.g. "the item store is unavailable". Empty
	// means [DefaultInternalErrorMessage]. It is the same for every failure:
	// that is the point.
	InternalErrorMessage Message

	// AllowMutatingTools lets [New] accept tools whose [ToolSpec.Access] is
	// [Mutating]. Set it only after the endpoint is behind authentication that
	// a write needs; see the package doc.
	AllowMutatingTools bool

	// AllowedOrigins lists the exact Origin header values (scheme://host[:port])
	// a request may carry. A request WITH an Origin header that is not listed is
	// refused with 403 before its body is read; a request with no Origin header
	// is unaffected. The MCP transport requires this check against DNS
	// rebinding: a browser page on an attacker's domain can otherwise POST to
	// a server on the victim's network. Agent clients running server-side send
	// no Origin, so the default -- nil, which refuses every browser origin --
	// costs them nothing. List an origin only for a browser-based client.
	AllowedOrigins []string
}

// Server is an MCP server over one HTTP POST endpoint. It is an
// [http.Handler]; it is safe for concurrent use and immutable after [New].
type Server struct {
	name         string
	version      string
	instructions string
	logger       *slog.Logger
	maxBody      int64
	internalMsg  Message
	origins      map[string]bool
	tools        []Tool
	byName       map[string]*Tool
}

// New builds a server exposing tools, in the order given. It returns every
// problem with the options and the tools joined into one error; each is a
// wiring bug, so a service should fail startup on it.
func New(opts Options, tools ...Tool) (*Server, error) {
	var errs []error
	if strings.TrimSpace(opts.Name) == "" {
		errs = append(errs, errors.New("mcp: Options.Name is required"))
	}
	if opts.MaxBodyBytes < 0 {
		errs = append(errs, errors.New("mcp: Options.MaxBodyBytes must not be negative"))
	}
	s := &Server{
		name:         opts.Name,
		version:      opts.Version,
		instructions: opts.Instructions,
		logger:       opts.Logger,
		maxBody:      opts.MaxBodyBytes,
		internalMsg:  opts.InternalErrorMessage,
		origins:      make(map[string]bool, len(opts.AllowedOrigins)),
		tools:        make([]Tool, 0, len(tools)),
		byName:       make(map[string]*Tool, len(tools)),
	}
	if s.version == "" {
		s.version = "0.0.0"
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.maxBody == 0 {
		s.maxBody = DefaultMaxBodyBytes
	}
	if s.internalMsg == "" {
		s.internalMsg = DefaultInternalErrorMessage
	}
	for _, o := range opts.AllowedOrigins {
		if o == "" || o == "*" || o == "null" || !strings.Contains(o, "://") || strings.HasSuffix(o, "/") {
			errs = append(errs, fmt.Errorf("mcp: AllowedOrigins entry %q must be an exact scheme://host[:port]", o))
			continue
		}
		s.origins[o] = true
	}

	seen := map[string]bool{}
	for i, t := range tools {
		switch {
		case t.err != nil:
			errs = append(errs, t.err)
			continue
		case t.call == nil:
			errs = append(errs, fmt.Errorf("mcp: tool #%d was not built with NewTool", i))
			continue
		case seen[t.spec.Name]:
			errs = append(errs, fmt.Errorf("mcp: tool %s is registered twice", t.spec.Name))
			continue
		case t.spec.Access == Mutating && !opts.AllowMutatingTools:
			errs = append(errs, fmt.Errorf("mcp: tool %s is Mutating but Options.AllowMutatingTools is false", t.spec.Name))
			continue
		}
		seen[t.spec.Name] = true
		s.tools = append(s.tools, t)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for i := range s.tools {
		s.byName[s.tools[i].spec.Name] = &s.tools[i]
	}
	return s, nil
}

// ToolNames returns the registered tool names in tools/list order. Assert on it
// in a service test so the surface only grows deliberately.
func (s *Server) ToolNames() []string {
	names := make([]string, len(s.tools))
	for i := range s.tools {
		names[i] = s.tools[i].spec.Name
	}
	return names
}

// rpcError is the JSON-RPC 2.0 error object. It never carries data.
type rpcError struct {
	Code    int     `json:"code"`
	Message Message `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content           []textContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

var nullID = json.RawMessage("null")

// ServeHTTP handles one JSON-RPC 2.0 request object.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if origin, ok := r.Header["Origin"]; ok && (len(origin) != 1 || !s.origins[origin[0]]) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody+1))
	_ = r.Body.Close()
	if err != nil {
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeParseError, "parse error: request body unreadable"))
		return
	}
	if int64(len(body)) > s.maxBody {
		writeResponse(w, http.StatusRequestEntityTooLarge, errorResponse(nullID, CodeInvalidRequest, "invalid request: body too large"))
		return
	}

	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) == 0:
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeParseError, "parse error: empty body"))
		return
	case !json.Valid(trimmed):
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeParseError, "parse error: body is not valid JSON"))
		return
	case trimmed[0] == '[':
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeInvalidRequest, "invalid request: batch requests are not supported"))
		return
	case trimmed[0] != '{':
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeInvalidRequest, "invalid request: body must be a JSON object"))
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		writeResponse(w, http.StatusOK, errorResponse(nullID, CodeParseError, "parse error: body is not valid JSON"))
		return
	}

	// The PRESENCE of "id", not its value, makes this a request rather than a
	// notification (JSON-RPC 2.0 section 4.1).
	idRaw, hasID := raw["id"]
	id := nullID
	if hasID {
		if !validID(idRaw) {
			writeResponse(w, http.StatusOK, errorResponse(nullID, CodeInvalidRequest, "invalid request: id must be a string, a number or null"))
			return
		}
		id = idRaw
	}

	var version string
	if v, ok := raw["jsonrpc"]; !ok || json.Unmarshal(v, &version) != nil || version != "2.0" {
		s.reply(w, hasID, errorResponse(id, CodeInvalidRequest, `invalid request: jsonrpc must be "2.0"`))
		return
	}
	var method string
	if m, ok := raw["method"]; !ok || json.Unmarshal(m, &method) != nil || method == "" {
		s.reply(w, hasID, errorResponse(id, CodeInvalidRequest, "invalid request: method must be a non-empty string"))
		return
	}

	if !hasID {
		// A notification is acknowledged and never dispatched. The only ones a
		// tool server receives (notifications/initialized, .../cancelled)
		// need no action, and anything else sent without an id -- a
		// tools/call, say -- must not run invisibly.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.dispatch(r.Context(), method, raw["params"])
	if rpcErr != nil {
		writeResponse(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Error: rpcErr})
		return
	}
	writeResponse(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// reply writes resp for a request, or a bare 202 for a notification: a
// notification never gets a body, even when it was malformed.
func (s *Server) reply(w http.ResponseWriter, hasID bool, resp rpcResponse) {
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeResponse(w, http.StatusOK, resp)
}

func validID(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

func errorResponse(id json.RawMessage, code int, msg Message) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeResponse(w http.ResponseWriter, status int, resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		// Every result is pre-marshaled or built from plain maps; this cannot
		// happen short of a bug here. Answer something well formed anyway.
		b, _ = json.Marshal(errorResponse(resp.ID, CodeInternalError, DefaultInternalErrorMessage))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params), nil
	case "ping", "notifications/initialized":
		// notifications/initialized is a notification in normal use and never
		// reaches here; answer it gracefully if a client sends it with an id.
		return map[string]any{}, nil
	case "tools/list":
		descs := make([]map[string]any, len(s.tools))
		for i := range s.tools {
			descs[i] = s.tools[i].descriptor()
		}
		return map[string]any{"tools": descs}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	default:
		// The method is NOT repeated: it is caller input.
		return nil, &rpcError{Code: CodeMethodNotFound, Message: "method not found"}
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p) // best effort: fall back to the default
	}
	version := DefaultProtocolVersion
	if slices.Contains(supportedProtocolVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	out := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	}
	if s.instructions != "" {
		out["instructions"] = s.instructions
	}
	return out
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "invalid params: expected {name, arguments}"}
	}
	t, ok := s.byName[p.Name]
	if !ok {
		// The name is NOT repeated: it is caller input.
		return nil, &rpcError{Code: CodeInvalidParams, Message: "unknown tool"}
	}
	args, err := t.checkArguments(p.Arguments)
	if err != nil {
		return nil, s.handlerError(ctx, t.spec.Name, err)
	}
	return s.invoke(ctx, t, args)
}

// invoke runs the handler AND renders its result under one recover. Rendering
// marshals the handler's data, which runs the handler's own MarshalJSON
// methods: a panic there is a handler panic too, and must not escape
// ServeHTTP -- net/http would log the panic value and drop the connection.
func (s *Server) invoke(ctx context.Context, t *Tool, args json.RawMessage) (out any, rpcErr *rpcError) {
	defer func() {
		if rec := recover(); rec != nil {
			// The panic value goes to the log only: it is arbitrary handler
			// state. The stack is what makes the log line actionable.
			s.logger.ErrorContext(ctx, "mcp tool call panicked",
				slog.String("tool", t.spec.Name),
				slog.String("panic", fmt.Sprint(rec)),
				slog.String("stack", string(debug.Stack())))
			out, rpcErr = nil, &rpcError{Code: CodeInternalError, Message: s.internalMsg}
		}
	}()
	res, err := t.call(ctx, args)
	if err != nil {
		return nil, s.handlerError(ctx, t.spec.Name, err)
	}
	return s.render(ctx, t, res)
}

// handlerError maps a handler error to the wire. Only an InvalidArgument
// reaches the client; everything else is logged and replaced.
//
// An InvalidArgument anywhere in the chain wins, so errors.Join(dbErr,
// InvalidArgument(...)) answers -32602. Whatever else the chain carried is not
// silently dropped: it is logged at debug.
func (s *Server) handlerError(ctx context.Context, tool string, err error) *rpcError {
	if ae, ok := asArgumentError(err); ok {
		if error(ae) != err { //nolint:errorlint // identity, not errors.Is: "is the chain MORE than the argument error?"
			s.logger.DebugContext(ctx, "mcp tool call rejected arguments; the error chain carried more",
				slog.String("tool", tool), slog.Any("error", err))
		}
		return &rpcError{Code: CodeInvalidParams, Message: "invalid arguments: " + ae.msg}
	}
	return s.internal(ctx, tool, err)
}

func (s *Server) internal(ctx context.Context, tool string, err error) *rpcError {
	s.logger.ErrorContext(ctx, "mcp tool call failed", slog.String("tool", tool), slog.Any("error", err))
	return &rpcError{Code: CodeInternalError, Message: s.internalMsg}
}

// render turns a handler's Result into the tools/call result. The text block
// is written HERE, from the count and the tool's noun, and nowhere else.
func (s *Server) render(ctx context.Context, t *Tool, res Result) (any, *rpcError) {
	switch res.kind {
	case resultNotFound:
		return toolResult{Content: []textContent{{Type: "text", Text: notFoundText}}, IsError: true}, nil
	case resultStructured:
		if res.count < 0 {
			return nil, s.internal(ctx, t.spec.Name, fmt.Errorf("mcp: Structured count %d is negative", res.count))
		}
		data, err := json.Marshal(res.data)
		if err != nil {
			return nil, s.internal(ctx, t.spec.Name, fmt.Errorf("mcp: marshal structuredContent: %w", err))
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, s.internal(ctx, t.spec.Name, errors.New("mcp: structuredContent must marshal to a JSON object"))
		}
		return toolResult{
			Content:           []textContent{{Type: "text", Text: successText(res.count, t.spec.Noun, t.spec.Note)}},
			StructuredContent: data,
		}, nil
	default:
		return nil, s.internal(ctx, t.spec.Name, errors.New("mcp: handler returned the zero Result with a nil error"))
	}
}
