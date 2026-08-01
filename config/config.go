// Package config loads 12-factor, environment-driven configuration into a
// service's own struct type.
//
// The reason this exists rather than a hand-rolled block of os.Getenv calls is
// error aggregation. A service that fatals on the first bad value forces the
// operator into a fix-run-fix loop, once per mistake, each iteration costing a
// container restart. Load reports every problem it finds in a single error, so
// one run tells the operator everything that is wrong.
//
// Two deliberate non-goals:
//
//   - OTel's environment variables (OTEL_*) are NOT re-declared here. The OTel
//     SDK reads them itself, and a second reader would drift from the spec.
//   - There is no facility for reading credentials from files or config values.
//     Secrets arrive only as environment variables, from an ESO-synced Secret.
//
// Usage:
//
//	type Config struct {
//	    Addr          string        `env:"SVC_ADDR" default:":8080"`
//	    APIKey        string        `env:"SVC_API_KEY" required:"true"`
//	    ShutdownDrain time.Duration `env:"SVC_SHUTDOWN_DRAIN" default:"20s"`
//	    Obs           obs.Config    // nested structs are walked
//	}
//
//	cfg, err := config.Load[Config]()
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Sentinel causes, so callers and tests can classify a failure without string
// matching. Every one of them is wrapped in a *FieldError carrying the env var
// name.
var (
	// ErrRequired reports a field tagged required:"true" for which neither the
	// environment nor a default supplied a value.
	ErrRequired = errors.New("required environment variable is not set")

	// ErrUnsupportedType reports a struct field whose Go type this package
	// cannot parse. It is a programming error in the service, not operator
	// error, but it surfaces the same way so it cannot be missed.
	ErrUnsupportedType = errors.New("unsupported config field type")
)

// FieldError is a failure attributed to one environment variable.
//
// It names the ENV VAR rather than the Go field because that is the knob the
// operator actually turns; "SVC_PORT" is actionable from a Helm values file,
// "Config.Port" is not.
type FieldError struct {
	// Name is the environment variable, e.g. "SVC_PORT".
	Name string
	// Value is the offending value, empty when nothing was set. Fields whose
	// name suggests a secret are not echoed back; see redactedValue.
	Value string
	// Err is the underlying cause, one of the sentinels above or a parse error.
	Err error
}

func (e *FieldError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("%s: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("%s=%q: %v", e.Name, e.Value, e.Err)
}

// Unwrap exposes the cause so errors.Is(err, config.ErrRequired) works through
// both the FieldError and the errors.Join that Load returns.
func (e *FieldError) Unwrap() error { return e.Err }

// Load reads the process environment into a fresh value of T.
//
// T must be a struct. Each field is bound by its `env:"NAME"` tag; fields with
// no such tag are left alone, except for nested structs, which are walked so a
// service can compose kit sub-configs (obs.Config, outbound.Config) into one
// type. Supported field types are string, int, int64, bool, time.Duration and
// []string (comma-separated, elements trimmed, empties dropped).
//
// Resolution order per field: environment value, then `default:"..."`, then the
// zero value. An environment variable set to the empty string counts as unset,
// because that is exactly what a ConfigMap or Helm template renders for a value
// nobody filled in, and treating it as a real value would silently defeat the
// default.
//
// On failure Load returns the ZERO value of T together with an errors.Join of
// one *FieldError per bad or missing variable. Callers get nothing usable on
// error by design: a half-populated config that starts a listener on the wrong
// port is worse than no config at all.
func Load[T any]() (T, error) {
	var cfg T
	v := reflect.ValueOf(&cfg).Elem()
	if v.Kind() != reflect.Struct {
		var zero T
		return zero, fmt.Errorf("config: Load requires a struct type, got %s", v.Type())
	}
	if errs := loadStruct(v); len(errs) > 0 {
		var zero T
		return zero, errors.Join(errs...)
	}
	return cfg, nil
}

// loadStruct populates v in place and returns every field error it found rather
// than stopping at the first. Recursion collects nested struct errors into the
// same flat slice, so the join the caller sees is one error per env var no
// matter how deeply the field is nested.
func loadStruct(v reflect.Value) []error {
	var errs []error
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, tagged := field.Tag.Lookup("env")

		if !tagged {
			// Untagged structs are sub-configs to walk; anything else is a
			// plain field the service manages itself.
			if fv := v.Field(i); fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(time.Time{}) {
				errs = append(errs, loadStruct(fv)...)
			}
			continue
		}
		if name == "" {
			errs = append(errs, fmt.Errorf("config: field %s has an empty env tag", field.Name))
			continue
		}
		if err := loadField(v.Field(i), field, name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// loadField resolves and assigns a single tagged field.
func loadField(fv reflect.Value, field reflect.StructField, name string) error {
	raw, ok := lookup(name)
	fromEnv := ok
	if !ok {
		raw, ok = field.Tag.Lookup("default")
		// A default of "" is indistinguishable from no default and means the
		// same thing here: leave the zero value.
		ok = ok && raw != ""
	}
	if !ok {
		if field.Tag.Get("required") == "true" {
			return &FieldError{Name: name, Err: ErrRequired}
		}
		return nil
	}
	if err := assign(fv, raw); err != nil {
		// A bad default is a service bug, but the operator still needs to know
		// which variable to override, so it is reported the same way. Values
		// that came from the environment are echoed; a bad literal default is
		// echoed too, since it is committed source, not a secret.
		return &FieldError{Name: name, Value: redactedValue(name, raw, fromEnv), Err: err}
	}
	return nil
}

// lookup reads one variable, treating empty as absent. Split out so the
// emptiness rule lives in exactly one place.
func lookup(name string) (string, bool) {
	val, ok := os.LookupEnv(name)
	if !ok || val == "" {
		return "", false
	}
	return val, true
}

// redactedValue keeps credentials out of logs. Parse failures on a variable
// whose name marks it as a secret would otherwise print the secret verbatim
// into whatever collects the startup error.
func redactedValue(name, raw string, fromEnv bool) string {
	if !fromEnv {
		return raw
	}
	upper := strings.ToUpper(name)
	for _, marker := range []string{"SECRET", "PASSWORD", "TOKEN", "KEY", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return "[redacted]"
		}
	}
	return raw
}

// assign parses raw into fv. The supported set is deliberately small: every
// type here has one obvious textual form, so an operator can predict what a
// value will do without reading this code.
func assign(fv reflect.Value, raw string) error {
	// time.Duration is an int64 underneath, so it must be matched by type
	// before the integer kinds get a chance to eat it.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			// time.ParseDuration's own message quotes the input back, which
			// would defeat redactedValue on a secret-named variable, so only
			// the accepted form is reported.
			return errors.New("invalid duration (want e.g. 500ms, 20s, 1m30s)")
		}
		fv.SetInt(int64(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
		return nil

	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer: %w", errParse(err))
		}
		fv.SetInt(n)
		return nil

	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean (want true/false, 1/0): %w", errParse(err))
		}
		fv.SetBool(b)
		return nil

	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("%w: %s", ErrUnsupportedType, fv.Type())
		}
		fv.Set(reflect.ValueOf(splitList(raw)))
		return nil

	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedType, fv.Type())
	}
}

// errParse keeps only strconv's reason ("invalid syntax", "value out of
// range") and discards its echo of the input, which FieldError has already
// printed -- redacted, if the variable name looks like a credential.
func errParse(err error) error {
	var ne *strconv.NumError
	if errors.As(err, &ne) {
		return ne.Err
	}
	return err
}

// splitList turns "a, b ,c" into []string{"a","b","c"}. Trimming and dropping
// empties makes a trailing comma or a line-wrapped YAML value harmless.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
