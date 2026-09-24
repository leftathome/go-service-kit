// Package mcp is a small, stdlib-only Model Context Protocol server: JSON-RPC
// 2.0 over a single HTTP POST endpoint, exposing a fixed set of TOOLS to an
// agent client.
//
// It exists because two services (nagus, then quark) each hand-rolled the same
// few hundred lines, and the second copy was already diverging from the first
// in exactly the places that matter for safety. This package is those lines
// once, with the hardening both copies learned the hard way turned into the
// DEFAULT behaviour of the API rather than a convention each tool author has to
// remember.
//
// # Scope
//
// Implemented: initialize, notifications/initialized (and every other
// notification, as a no-op), ping, tools/list and tools/call. That is the whole
// surface a read-only tool server needs.
//
// Not implemented, deliberately: batch requests (refused with -32600),
// resources, prompts, sampling, server-to-client requests, SSE streaming and
// session ids. A GET on the endpoint is answered 405, which the streamable HTTP
// transport defines as "this server offers no SSE stream".
//
// # What the defaults enforce
//
// An agent client may place a tool result's TEXT content block straight into a
// model's context. Anything in that block can act as an instruction. Every
// protection below exists to keep attacker-authored text -- a seller's listing
// title, a product name, the caller's own arguments -- out of that block and
// out of error messages, and none of them is opt-in:
//
//   - Tool output is STRUCTURED ONLY. A handler returns [Structured] (a count
//     and a JSON object) or [NotFound]. It has no way to supply text. The
//     library writes the text block itself, from the count and a noun fixed at
//     registration: "3 result(s). The data is in structuredContent; ...". The
//     text block therefore never contains a structured value or a caller
//     argument, by construction.
//
//   - Internal failures are REDACTED. A handler error that is not an
//     [InvalidArgument] becomes JSON-RPC -32603 with a fixed message
//     ([Options.InternalErrorMessage]); the error itself is logged, with the
//     tool name, through [Options.Logger]. A store error routinely carries a
//     DSN, a file path or a query fragment. A handler panic is treated the
//     same way.
//
//   - Client-error messages are CONSTANTS. [InvalidArgument] takes a [Message],
//     a named string type: an untyped string constant converts to it
//     implicitly, but a string built at runtime ("bad id: " + id) does not
//     compile without an explicit conversion, which is the point at which a
//     reviewer should ask whether caller input is being echoed.
//
//   - Protocol errors never echo input. "method not found" does not repeat the
//     method; "unknown tool" does not repeat the tool name; a parse error does
//     not quote the decoder's message, which can contain request bytes.
//
//   - Arguments are decoded STRICTLY. [NewTool] is the only way to build a
//     [Tool], and it binds the handler to a struct type. The arguments object
//     must be a JSON object whose keys are exactly properties of the input
//     schema (case-sensitively -- encoding/json alone would accept "ID" for
//     "id"), unknown fields are rejected at every depth, trailing data is
//     rejected, and every "required" key must be present. The schema's
//     additionalProperties is forced to false, and [New] refuses a schema whose
//     properties do not match the struct's JSON fields in both directions, so
//     what tools/list advertises is exactly what tools/call accepts.
//
//   - Tools are READ-ONLY unless the server opts in. [ToolSpec.Mutating] is
//     false by default and [New] refuses a mutating tool unless
//     [Options.AllowMutatingTools] is set. The reason is operational, not
//     cosmetic: a service typically exempts its MCP endpoint from bearer
//     authentication BECAUSE every tool is a read. A write must not inherit
//     that exemption by being added to a list. tools/list advertises the
//     choice as annotations.readOnlyHint.
//
//   - Requests are bounded. The body is capped at [Options.MaxBodyBytes]
//     (default [DefaultMaxBodyBytes]); larger requests get HTTP 413.
//
//   - Notifications never run tools. A request without an id is answered 202
//     with no body and is not dispatched to a tool, so a tools/call sent as a
//     notification has no effect instead of an invisible one.
//
// What the library CANNOT enforce: the contents of the structured object. If a
// value in it is untrusted, mark it so (quark wraps every free-text value in an
// {"untrusted": true, "value": ...} envelope) and say so in the tool's
// description. The text block tells the agent the free-text values are
// untrusted; the envelope is what makes that true value by value.
//
// # Usage
//
//	type getArgs struct {
//	    ID string `json:"id"`
//	}
//
//	get := mcp.NewTool(mcp.ToolSpec{
//	    Name:        "get_item",
//	    Description: "READ-ONLY fetch of one item by id.",
//	    InputSchema: map[string]any{
//	        "type":       "object",
//	        "properties": map[string]any{"id": map[string]any{"type": "string"}},
//	        "required":   []string{"id"},
//	    },
//	    Noun: "item(s)",
//	}, func(ctx context.Context, a getArgs) (mcp.Result, error) {
//	    if a.ID == "" {
//	        return mcp.Result{}, mcp.InvalidArgument("id is required")
//	    }
//	    it, ok, err := store.Get(ctx, a.ID)
//	    if err != nil {
//	        return mcp.Result{}, err // logged; caller sees the fixed message
//	    }
//	    if !ok {
//	        return mcp.NotFound(), nil
//	    }
//	    return mcp.Structured(1, map[string]any{"item": it}), nil
//	})
//
//	srv, err := mcp.New(mcp.Options{
//	    Name: "nagus", Version: version, Logger: logger,
//	    InternalErrorMessage: "the item store is unavailable",
//	}, get, search)
//	if err != nil { ... } // a wiring bug: fail startup
//	api.RawRoute("POST /mcp", srv)
//
// Mount it with [github.com/leftathome/go-service-kit/httpapi.API.RawRoute]:
// the route is instrumented and traced like any other, absent from the OpenAPI
// document, and listed by RawRoutes as a deliberate exception to the
// problem+json error format.
//
// # Compatibility notes
//
// Invalid tool arguments are a JSON-RPC -32602 error, as in the servers this
// package replaces, not a tool-level isError result. A missing entity IS a
// tool-level result ([NotFound]): the call succeeded and found nothing.
//
// initialize answers with the client's protocolVersion when it is one of
// [SupportedProtocolVersions], and with [DefaultProtocolVersion] otherwise, as
// the MCP lifecycle specifies. (The hand-rolled servers echoed any string.)
package mcp
