package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// obsConfig stands in for a nested kit sub-config (obs.Config in real use), so
// the tests pin down that Load recurses into embedded service structs.
type obsConfig struct {
	ServiceName string `env:"SVC_OBS_SERVICE_NAME" default:"servicename"`
	SampleRatio string `env:"SVC_OBS_SAMPLE_RATIO"`
}

type testConfig struct {
	Addr             string        `env:"SVC_ADDR"          default:":8080"`
	Port             int           `env:"SVC_PORT"          default:"8080"`
	MaxBytes         int64         `env:"SVC_MAX_BYTES"     default:"1048576"`
	DocsEnabled      bool          `env:"SVC_DOCS_ENABLED"  default:"true"`
	ShutdownDrain    time.Duration `env:"SVC_SHUTDOWN_DRAIN" default:"20s"`
	AllowedOrigins   []string      `env:"SVC_ALLOWED_ORIGINS" default:"a.example,b.example"`
	Obs              obsConfig
	unexportedIgnore string //nolint:unused // proves unexported fields are skipped
	Computed         string // no env tag: not config-managed
}

func TestLoadAppliesDefaultsWhenEnvUnset(t *testing.T) {
	cfg, err := Load[testConfig]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := testConfig{
		Addr:           ":8080",
		Port:           8080,
		MaxBytes:       1048576,
		DocsEnabled:    true,
		ShutdownDrain:  20 * time.Second,
		AllowedOrigins: []string{"a.example", "b.example"},
		Obs:            obsConfig{ServiceName: "servicename"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadParsesEverySupportedType(t *testing.T) {
	t.Setenv("SVC_ADDR", "127.0.0.1:9999")
	t.Setenv("SVC_PORT", "9999")
	t.Setenv("SVC_MAX_BYTES", "9223372036854775807")
	t.Setenv("SVC_DOCS_ENABLED", "false")
	t.Setenv("SVC_SHUTDOWN_DRAIN", "1m30s")
	t.Setenv("SVC_ALLOWED_ORIGINS", "one.example, two.example ,three.example")
	t.Setenv("SVC_OBS_SERVICE_NAME", "widgetd")

	cfg, err := Load[testConfig]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := testConfig{
		Addr:           "127.0.0.1:9999",
		Port:           9999,
		MaxBytes:       9223372036854775807,
		DocsEnabled:    false,
		ShutdownDrain:  90 * time.Second,
		AllowedOrigins: []string{"one.example", "two.example", "three.example"},
		Obs:            obsConfig{ServiceName: "widgetd"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadAggregatesAllErrors(t *testing.T) {
	t.Setenv("SVC_PORT", "not-a-number")
	t.Setenv("SVC_SHUTDOWN_DRAIN", "also-bad")
	_, err := Load[testConfig]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	// Both bad fields must be reported, so one fix-run-fix cycle finds them all.
	for _, want := range []string{"SVC_PORT", "SVC_SHUTDOWN_DRAIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// N invalid fields must yield exactly N joined errors, not a single summary the
// operator has to re-run to unpack.
func TestLoadReportsOneErrorPerBadField(t *testing.T) {
	t.Setenv("SVC_PORT", "not-a-number")
	t.Setenv("SVC_MAX_BYTES", "huge")
	t.Setenv("SVC_DOCS_ENABLED", "yes-please")
	t.Setenv("SVC_SHUTDOWN_DRAIN", "also-bad")

	_, err := Load[testConfig]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	joined, ok := err.(interface{ Unwrap() []error }) //nolint:errorlint // asserting on the join itself
	if !ok {
		t.Fatalf("Load error is not a join of errors: %T", err)
	}
	if got, want := len(joined.Unwrap()), 4; got != want {
		t.Errorf("got %d errors, want %d: %v", got, want, err)
	}
}

func TestLoadNamesTheEnvVarNotTheGoField(t *testing.T) {
	t.Setenv("SVC_PORT", "not-a-number")
	_, err := Load[testConfig]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "SVC_PORT") {
		t.Errorf("error must name the env var the operator sets: %v", err)
	}
	if strings.Contains(err.Error(), "Port ") || strings.Contains(err.Error(), "testConfig") {
		t.Errorf("error leaks Go field naming: %v", err)
	}
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("want a *FieldError in the chain, got %v", err)
	}
	if fe.Name != "SVC_PORT" {
		t.Errorf("FieldError.Name = %q, want %q", fe.Name, "SVC_PORT")
	}
	if fe.Value != "not-a-number" {
		t.Errorf("FieldError.Value = %q, want %q", fe.Value, "not-a-number")
	}
}

func TestLoadLeavesZeroValueWhenUnsetAndNoDefault(t *testing.T) {
	type c struct {
		Optional string        `env:"SVC_OPTIONAL"`
		Count    int           `env:"SVC_COUNT"`
		Every    time.Duration `env:"SVC_EVERY"`
		Hosts    []string      `env:"SVC_HOSTS"`
	}
	got, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, c{}) {
		t.Errorf("Load() = %+v, want zero value", got)
	}
}

func TestLoadRequiredMissingIsAnError(t *testing.T) {
	type c struct {
		APIKey string `env:"SVC_API_KEY" required:"true"`
		DBURL  string `env:"SVC_DB_URL"  required:"true"`
	}
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"SVC_API_KEY", "SVC_DB_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if !errors.Is(err, ErrRequired) {
		t.Errorf("want ErrRequired in the chain, got %v", err)
	}
}

func TestLoadRequiredSatisfiedByEnvOrDefault(t *testing.T) {
	type c struct {
		APIKey string `env:"SVC_API_KEY" required:"true"`
		Region string `env:"SVC_REGION"  required:"true" default:"us-east"`
	}
	t.Setenv("SVC_API_KEY", "sekrit")
	got, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.APIKey != "sekrit" || got.Region != "us-east" {
		t.Errorf("Load() = %+v", got)
	}
}

// An env var set to the empty string is what a ConfigMap or Helm template
// renders for a value nobody filled in, so it must behave as "unset".
func TestLoadTreatsEmptyEnvAsUnset(t *testing.T) {
	type c struct {
		Addr   string `env:"SVC_ADDR" default:":8080"`
		APIKey string `env:"SVC_API_KEY" required:"true"`
	}
	t.Setenv("SVC_ADDR", "")
	t.Setenv("SVC_API_KEY", "")
	if _, err := Load[c](); err == nil {
		t.Fatal("want error for empty required var, got nil")
	} else if !errors.Is(err, ErrRequired) {
		t.Errorf("want ErrRequired, got %v", err)
	}
}

// A parse failure must not print the secret it failed to parse into the
// startup log, which is typically shipped somewhere less trusted than the pod.
func TestLoadRedactsSecretValuesInErrors(t *testing.T) {
	type c struct {
		Timeout time.Duration `env:"SVC_API_TOKEN_TTL"`
	}
	t.Setenv("SVC_API_TOKEN_TTL", "hunter2")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes a secret-looking value: %v", err)
	}
	if !strings.Contains(err.Error(), "SVC_API_TOKEN_TTL") {
		t.Errorf("error missing SVC_API_TOKEN_TTL: %v", err)
	}
}

func TestLoadEmptyEnvAppliesDefault(t *testing.T) {
	type c struct {
		Addr string `env:"SVC_ADDR" default:":8080"`
	}
	t.Setenv("SVC_ADDR", "")
	got, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", got.Addr, ":8080")
	}
}

func TestLoadStringSliceTrimsAndDropsEmpties(t *testing.T) {
	type c struct {
		Hosts []string `env:"SVC_HOSTS"`
	}
	t.Setenv("SVC_HOSTS", " a , ,b,, c ")
	got, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got.Hosts, want) {
		t.Errorf("Hosts = %#v, want %#v", got.Hosts, want)
	}
}

func TestLoadBadDefaultIsReportedAgainstTheEnvVar(t *testing.T) {
	type c struct {
		Port int `env:"SVC_PORT" default:"eighty"`
	}
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want error for unparseable default, got nil")
	}
	if !strings.Contains(err.Error(), "SVC_PORT") {
		t.Errorf("error missing SVC_PORT: %v", err)
	}
}

func TestLoadUnsupportedFieldTypeIsAnError(t *testing.T) {
	type c struct {
		Ratio float64 `env:"SVC_RATIO"`
	}
	t.Setenv("SVC_RATIO", "0.5")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want error for unsupported field type, got nil")
	}
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("want ErrUnsupportedType, got %v", err)
	}
	if !strings.Contains(err.Error(), "SVC_RATIO") {
		t.Errorf("error missing SVC_RATIO: %v", err)
	}
}

func TestLoadNonStructTypeIsAnError(t *testing.T) {
	if _, err := Load[int](); err == nil {
		t.Fatal("want error when T is not a struct, got nil")
	}
}

func TestLoadReturnsZeroValueOnError(t *testing.T) {
	type c struct {
		Addr string `env:"SVC_ADDR" default:":8080"`
		Port int    `env:"SVC_PORT"`
	}
	t.Setenv("SVC_PORT", "nope")
	got, err := Load[c]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	// A half-populated config is worse than none: callers must not be able to
	// start a server on a partially valid struct.
	if !reflect.DeepEqual(got, c{}) {
		t.Errorf("Load() = %+v on error, want zero value", got)
	}
}

func TestLoadBoolAcceptsStrconvForms(t *testing.T) {
	type c struct {
		Flag bool `env:"SVC_FLAG"`
	}
	for _, raw := range []string{"1", "t", "T", "true", "TRUE", "True"} {
		t.Setenv("SVC_FLAG", raw)
		got, err := Load[c]()
		if err != nil {
			t.Fatalf("Load(%q): %v", raw, err)
		}
		if !got.Flag {
			t.Errorf("Load(%q).Flag = false, want true", raw)
		}
	}
	for _, raw := range []string{"0", "f", "false", "FALSE"} {
		t.Setenv("SVC_FLAG", raw)
		got, err := Load[c]()
		if err != nil {
			t.Fatalf("Load(%q): %v", raw, err)
		}
		if got.Flag {
			t.Errorf("Load(%q).Flag = true, want false", raw)
		}
	}
}

func TestLoadNestedStructErrorsAreAggregatedToo(t *testing.T) {
	type inner struct {
		Retries int `env:"SVC_INNER_RETRIES"`
	}
	type outer struct {
		Port  int `env:"SVC_PORT"`
		Inner inner
	}
	t.Setenv("SVC_PORT", "bad")
	t.Setenv("SVC_INNER_RETRIES", "worse")

	_, err := Load[outer]()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	joined, ok := err.(interface{ Unwrap() []error }) //nolint:errorlint // asserting on the join itself
	if !ok {
		t.Fatalf("Load error is not a join of errors: %T", err)
	}
	if got := len(joined.Unwrap()); got != 2 {
		t.Errorf("got %d errors, want 2: %v", got, err)
	}
	for _, want := range []string{"SVC_PORT", "SVC_INNER_RETRIES"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestFieldErrorUnwrapsToCause(t *testing.T) {
	fe := &FieldError{Name: "SVC_PORT", Value: "x", Err: ErrRequired}
	if !errors.Is(fe, ErrRequired) {
		t.Error("FieldError must unwrap to its cause")
	}
	if !strings.Contains(fe.Error(), "SVC_PORT") {
		t.Errorf("FieldError.Error() must name the env var: %s", fe.Error())
	}
}
