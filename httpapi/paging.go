package httpapi

// House pagination. One envelope, one parameter pair, one set of bounds, for
// every list endpoint in every service built on the kit.
//
// # Why cursors and not offsets
//
// Offset pagination re-scans the skipped rows on every page and silently skips
// or repeats items when the underlying data changes mid-walk. Both problems
// get worse exactly as a dataset grows into the size where paging matters. A
// cursor encodes a position in a stable order, so page N+1 costs the same as
// page 1 and a concurrent insert cannot shift the window underneath a client.
//
// # The cursor is opaque
//
// A cursor is produced by the store and handed straight back. Clients must not
// parse, construct, or persist one across a deployment. Adapters should encode
// rather than expose a raw key, so that changing the underlying order is not a
// breaking API change. This is the same contract as storekit.Cursor, and
// [NewPage] accepts that type directly -- without an import edge, because no
// kit package imports another.

const (
	// DefaultPageSize is the page size when the client does not ask.
	DefaultPageSize = 50

	// MaxPageSize is the largest page a client may request. It bounds the work
	// one request can ask the store to do; a service with cheap rows may
	// document a higher cap of its own, but not by widening this one.
	MaxPageSize = 200

	// MaxCursorLength bounds an inbound cursor. A cursor is opaque but it is
	// still attacker-controlled input that a store adapter will decode, so it
	// is rejected by schema validation before any handler sees it.
	MaxCursorLength = 1024
)

// Page is the house response envelope for every list endpoint.
//
//	{"items": [...], "next_cursor": "...", "has_more": true}
//
// Items is never null: an empty page serializes as [], because null.length is
// the single most common client-side crash in a paginated API.
//
// HasMore is exactly (NextCursor != ""). It is redundant on purpose -- clients
// consistently get "is there another page" wrong when the only signal is the
// emptiness of a string they are told not to look at.
type Page[T any] struct {
	// nullable:"false" makes the SCHEMA match the guarantee. huma's default is
	// that a slice may be null, and a schema that says "array or null" while
	// the code never emits null trains clients to write the null branch --
	// or, worse, to stop trusting the schema.
	Items      []T    `json:"items" nullable:"false" doc:"The page of results, in a stable order. Never null."`
	NextCursor string `json:"next_cursor,omitempty" doc:"Opaque cursor for the next page. Absent on the last page. Do not parse it."`
	HasMore    bool   `json:"has_more" doc:"Whether a further page exists. Equivalent to next_cursor being present."`
}

// NewPage builds the envelope. The cursor type parameter is a ~string
// constraint rather than a plain string so that storekit.Cursor -- what a
// store adapter's List actually returns -- passes through without a conversion
// at every call site and without either package importing the other.
//
//	items, next, err := store.List(ctx, storekit.Cursor(in.Cursor))
//	return &httpapi.PageResponse[Widget]{Body: httpapi.NewPage(items, next)}, nil
func NewPage[T any, C ~string](items []T, next C) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{
		Items:      items,
		NextCursor: string(next),
		HasMore:    next != "",
	}
}

// PageResponse is the huma output wrapper for a paginated endpoint, so a
// handler does not spell out `struct{ Body Page[T] }` in every file.
type PageResponse[T any] struct {
	Body Page[T]
}

// PageParams is the house query-parameter pair. Embed it in a huma input
// struct to get `cursor` and `limit` with the bounds already applied:
//
//	type ListWidgetsInput struct {
//	    httpapi.PageParams
//	    Owner string `query:"owner"`
//	}
//
// The bounds are enforced by SCHEMA VALIDATION, so limit=100000 is a 422 with
// a per-field location rather than a silent clamp. Silent clamping is how a
// client ends up paging forever without noticing that it never got the page
// size it asked for.
type PageParams struct {
	Cursor string `query:"cursor" maxLength:"1024" doc:"Opaque cursor from a previous response's next_cursor. Omit for the first page."`
	Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50" doc:"Maximum items to return."`
}

// EffectiveLimit resolves the limit a handler should pass to the store.
//
// Schema validation has already rejected anything outside [1, MaxPageSize] on
// a huma-routed request, so in practice this only substitutes the default for
// an unset value. It clamps anyway, because a handler may be called from a
// test or from a non-huma path, and a store must never be handed a limit of
// zero or a limit of a million.
func (p PageParams) EffectiveLimit() int {
	switch {
	case p.Limit <= 0:
		return DefaultPageSize
	case p.Limit > MaxPageSize:
		return MaxPageSize
	default:
		return p.Limit
	}
}
