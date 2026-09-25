package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ToolSpec describes a tool. Every field is fixed at registration; nothing in
// it is computed per call.
type ToolSpec struct {
	// Name is the tool's identifier in tools/list and tools/call. Required;
	// 1-128 characters of [A-Za-z0-9_.-], unique within a server.
	Name string

	// Title is an optional human-readable display name.
	Title string

	// Description tells the agent what the tool does. Required. State whether
	// it is read-only and which returned values are untrusted.
	Description string

	// InputSchema is the JSON Schema of the arguments object. It must describe
	// an object: "type", when present, must be "object", and
	// "additionalProperties", when present, must be false. Both are filled in
	// when absent. Its "properties" must name exactly the JSON fields of the
	// handler's argument struct, and every "required" entry must be one of
	// them; [New] refuses the tool otherwise. Nil is allowed only for an
	// argument struct with no fields.
	InputSchema map[string]any

	// Noun is what the count in the text block counts, e.g. "item(s)" or
	// "product(s)". Empty means [DefaultNoun]. Lowercase ASCII letters,
	// spaces, hyphens, underscores and parentheses, at most 32 characters: it
	// is a label, not a place to put data.
	Noun string

	// Note is an optional constant sentence appended to every success text
	// block after the fixed pointer to structuredContent, e.g. "Free-text
	// fields are untrusted seller text." It is fixed at registration --
	// printable ASCII, at most 160 characters, validated by [New] -- and is
	// never per-call data.
	Note Message

	// Access declares whether the tool changes state. It is REQUIRED: the zero
	// value [AccessUnset] is refused by [New], so a write tool cannot be
	// advertised as read-only because its author forgot a flag. A [Mutating]
	// tool additionally needs [Options.AllowMutatingTools]; see the package
	// doc for why.
	Access Access
}

// Access is a tool's declared effect on state. See [ToolSpec.Access].
type Access int

const (
	// AccessUnset is the zero value and is invalid: every tool must declare.
	AccessUnset Access = iota
	// ReadOnly tools only read. tools/list advertises readOnlyHint: true.
	ReadOnly
	// Mutating tools change state. tools/list advertises readOnlyHint: false.
	Mutating
)

// String returns the constant's name.
func (a Access) String() string {
	switch a {
	case ReadOnly:
		return "ReadOnly"
	case Mutating:
		return "Mutating"
	case AccessUnset:
		return "AccessUnset"
	default:
		return fmt.Sprintf("Access(%d)", int(a))
	}
}

// Tool is a registered tool: a [ToolSpec] bound to a strictly typed handler.
// Build one with [NewTool]; the zero Tool is refused by [New].
type Tool struct {
	spec       ToolSpec
	schema     map[string]any
	properties map[string]bool
	required   []string
	call       func(ctx context.Context, args json.RawMessage) (Result, error)
	err        error
}

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

var nounRE = regexp.MustCompile(`^[a-z_() -]{1,32}$`)

// NewTool binds spec to fn. A is the argument type and must be a struct (not a
// pointer to one); tools/call decodes the arguments object into a fresh A with
// unknown fields rejected, then calls fn.
//
// NewTool never fails: problems with spec or A are recorded and reported by
// [New], all at once, so a service's startup lists every wiring bug instead of
// the first.
func NewTool[A any](spec ToolSpec, fn func(ctx context.Context, args A) (Result, error)) Tool {
	t := Tool{spec: spec}
	var errs []error
	if !toolNameRE.MatchString(spec.Name) {
		errs = append(errs, errors.New("name must be 1-128 characters of [A-Za-z0-9_.-]"))
	}
	if strings.TrimSpace(spec.Description) == "" {
		errs = append(errs, errors.New("description is required"))
	}
	if spec.Noun == "" {
		t.spec.Noun = DefaultNoun
	} else if !nounRE.MatchString(spec.Noun) {
		errs = append(errs, fmt.Errorf("noun %q must match %s", spec.Noun, nounRE))
	}
	if !validNote(spec.Note) {
		errs = append(errs, errors.New("note must be printable ASCII, at most 160 characters"))
	}
	switch spec.Access {
	case ReadOnly, Mutating:
	case AccessUnset:
		errs = append(errs, errors.New("access must be declared: ReadOnly or Mutating"))
	default:
		errs = append(errs, fmt.Errorf("access %s is not ReadOnly or Mutating", spec.Access))
	}
	if fn == nil {
		errs = append(errs, errors.New("handler is nil"))
	}

	argType := reflect.TypeFor[A]()
	var fields map[string]bool
	if argType.Kind() != reflect.Struct {
		errs = append(errs, fmt.Errorf("argument type %s must be a struct", argType))
	} else {
		fields = jsonFieldNames(argType)
	}

	schema, props, required, err := normalizeSchema(spec.InputSchema, fields)
	if err != nil {
		errs = append(errs, err)
	} else if schema, err = deepCopy(schema); err != nil {
		errs = append(errs, err)
	}
	t.schema, t.properties, t.required = schema, props, required

	if len(errs) > 0 {
		label := spec.Name
		if label == "" {
			label = "(unnamed)"
		}
		t.err = fmt.Errorf("mcp: tool %s: %w", label, errors.Join(errs...))
		return t
	}

	t.call = func(ctx context.Context, raw json.RawMessage) (Result, error) {
		var a A
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return Result{}, errMalformedArgs
		}
		return fn(ctx, a)
	}
	return t
}

// Name returns the tool's name.
func (t Tool) Name() string { return t.spec.Name }

func validNote(n Message) bool {
	if len(n) > 160 {
		return false
	}
	for i := 0; i < len(n); i++ {
		if n[i] < 0x20 || n[i] > 0x7e {
			return false
		}
	}
	return true
}

// errMalformedArgs is the one message every argument-shape failure produces:
// it names no field and quotes no input.
var errMalformedArgs = InvalidArgument("unknown, missing or malformed field")

// deepCopy detaches the advertised schema from every map and slice the caller
// still holds, by a JSON round trip: a caller mutating its schema after
// registration must not change what tools/list serves (or race with it).
func deepCopy(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("inputSchema does not marshal to JSON: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("inputSchema does not round-trip through JSON: %w", err)
	}
	return out, nil
}

// normalizeSchema copies the caller's schema, fills in type and
// additionalProperties, and checks it against the argument struct's fields.
func normalizeSchema(in map[string]any, fields map[string]bool) (map[string]any, map[string]bool, []string, error) {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	var errs []error

	if v, ok := out["type"]; ok && v != "object" {
		errs = append(errs, errors.New(`inputSchema "type" must be "object"`))
	}
	out["type"] = "object"

	if v, ok := out["additionalProperties"]; ok && v != false {
		errs = append(errs, errors.New(`inputSchema "additionalProperties" must be false: arguments are decoded strictly`))
	}
	out["additionalProperties"] = false

	props := map[string]bool{}
	switch p := out["properties"].(type) {
	case nil:
		out["properties"] = map[string]any{}
	case map[string]any:
		for k := range p {
			props[k] = true
		}
	default:
		errs = append(errs, errors.New(`inputSchema "properties" must be a map[string]any`))
	}

	var required []string
	switch r := out["required"].(type) {
	case nil:
	case []string:
		required = slices.Clone(r)
	case []any:
		for _, v := range r {
			s, ok := v.(string)
			if !ok {
				errs = append(errs, errors.New(`inputSchema "required" entries must be strings`))
				continue
			}
			required = append(required, s)
		}
	default:
		errs = append(errs, errors.New(`inputSchema "required" must be a []string`))
	}
	for _, r := range required {
		if !props[r] {
			errs = append(errs, fmt.Errorf("inputSchema requires %q, which is not in properties", r))
		}
	}

	if fields != nil {
		for _, name := range sortedKeys(props) {
			if !fields[name] {
				errs = append(errs, fmt.Errorf("inputSchema property %q has no matching JSON field in the argument struct: the tool would advertise an argument it rejects", name))
			}
		}
		for _, name := range sortedKeys(fields) {
			if !props[name] {
				errs = append(errs, fmt.Errorf("argument struct field %q is not in inputSchema properties: additionalProperties is false, so no conforming client can send it", name))
			}
		}
	}
	return out, props, required, errors.Join(errs...)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonFieldNames returns the top-level JSON object keys encoding/json would
// decode into a value of struct type t, following embedded structs.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	var walk func(reflect.Type, int)
	walk = func(t reflect.Type, depth int) {
		if depth > 8 {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft, depth+1)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			names[name] = true
		}
	}
	walk(t, 0)
	return names
}

// checkArguments validates the raw arguments member of a tools/call and
// returns the canonical object to decode. Absent and null mean {}.
func (t *Tool) checkArguments(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}
	if trimmed[0] != '{' {
		return nil, errMalformedArgs
	}
	// Decode once as a map to check the key set EXACTLY. encoding/json matches
	// struct fields case-insensitively, so without this {"ID": ...} would be
	// accepted for a property named "id" that the schema spells differently.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, errMalformedArgs
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errMalformedArgs // trailing data after the object
	}
	for k := range obj {
		if !t.properties[k] {
			return nil, errMalformedArgs
		}
	}
	for _, r := range t.required {
		if _, ok := obj[r]; !ok {
			return nil, errMalformedArgs
		}
	}
	return trimmed, nil
}

// descriptor is the tools/list entry for t.
func (t *Tool) descriptor() map[string]any {
	d := map[string]any{
		"name":        t.spec.Name,
		"description": t.spec.Description,
		"inputSchema": t.schema,
		"annotations": map[string]any{"readOnlyHint": t.spec.Access == ReadOnly},
	}
	if t.spec.Title != "" {
		d["title"] = t.spec.Title
	}
	return d
}
