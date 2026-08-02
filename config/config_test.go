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
		Labels map[string]string `env:"SVC_LABELS"`
	}
	t.Setenv("SVC_LABELS", "a=b")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want error for unsupported field type, got nil")
	}
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("want ErrUnsupportedType, got %v", err)
	}
	// The env var name must survive into the message: an operator reading a
	// crash log needs the knob, not the Go type alone.
	if !strings.Contains(err.Error(), "SVC_LABELS") {
		t.Errorf("error missing SVC_LABELS: %v", err)
	}
}

// --- floats -----------------------------------------------------------------
//
// nagus could not adopt this package at all without float64: NAGUS_MIN_CAPACITY
// (terabytes) and NAGUS_LAND_{MIN,MAX}_ACREAGE are all fractional thresholds.

func TestLoadParsesFloats(t *testing.T) {
	type c struct {
		MinCapacity float64 `env:"SVC_MIN_CAPACITY"`
		MinAcreage  float64 `env:"SVC_MIN_ACREAGE" default:"0.25"`
		Ratio       float32 `env:"SVC_RATIO"`
	}
	t.Setenv("SVC_MIN_CAPACITY", "17.5")
	t.Setenv("SVC_RATIO", "-2.5e-3")

	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinCapacity != 17.5 {
		t.Errorf("MinCapacity = %v, want 17.5", cfg.MinCapacity)
	}
	if cfg.MinAcreage != 0.25 {
		t.Errorf("MinAcreage = %v, want 0.25 (from default)", cfg.MinAcreage)
	}
	if cfg.Ratio != -2.5e-3 {
		t.Errorf("Ratio = %v, want -0.0025", cfg.Ratio)
	}
}

func TestLoadRejectsNonFiniteFloats(t *testing.T) {
	type c struct {
		Threshold float64 `env:"SVC_THRESHOLD"`
	}
	// A NaN threshold makes every comparison against it false, silently: the
	// service starts, filters nothing, and looks healthy. Refuse it at boot.
	for _, raw := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity"} {
		t.Setenv("SVC_THRESHOLD", raw)
		if _, err := Load[c](); err == nil {
			t.Errorf("Load(%q) succeeded, want an error", raw)
		}
	}
}

func TestLoadRejectsFloatOutOfRangeForWidth(t *testing.T) {
	type c struct {
		Small float32 `env:"SVC_SMALL"`
	}
	t.Setenv("SVC_SMALL", "1e40")
	if _, err := Load[c](); err == nil {
		t.Fatal("want an out-of-range error for a float32, got nil")
	}
}

// --- unsigned and narrow integers -------------------------------------------

func TestLoadParsesUnsignedAndNarrowIntegers(t *testing.T) {
	type c struct {
		Workers  uint   `env:"SVC_WORKERS"`
		MaxBytes uint64 `env:"SVC_MAX_BYTES"`
		Weight   uint8  `env:"SVC_WEIGHT"`
		Offset   int32  `env:"SVC_OFFSET"`
	}
	t.Setenv("SVC_WORKERS", "4")
	t.Setenv("SVC_MAX_BYTES", "18446744073709551615")
	t.Setenv("SVC_WEIGHT", "255")
	t.Setenv("SVC_OFFSET", "-2147483648")

	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := c{Workers: 4, MaxBytes: 18446744073709551615, Weight: 255, Offset: -2147483648}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadRejectsNegativeUnsigned(t *testing.T) {
	type c struct {
		Workers uint `env:"SVC_WORKERS"`
	}
	t.Setenv("SVC_WORKERS", "-1")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want an error for a negative unsigned value, got nil")
	}
	if !strings.Contains(err.Error(), "SVC_WORKERS") {
		t.Errorf("error missing SVC_WORKERS: %v", err)
	}
}

func TestLoadRejectsOverflowingNarrowInteger(t *testing.T) {
	type c struct {
		Weight uint8 `env:"SVC_WEIGHT"`
	}
	t.Setenv("SVC_WEIGHT", "256")
	if _, err := Load[c](); err == nil {
		t.Fatal("want an out-of-range error for uint8, got nil")
	}
}

// --- time.Time ---------------------------------------------------------------

func TestLoadParsesRFC3339Time(t *testing.T) {
	type c struct {
		CutoverAt time.Time `env:"SVC_CUTOVER_AT"`
		Epoch     time.Time `env:"SVC_EPOCH" default:"2026-01-01T00:00:00Z"`
	}
	t.Setenv("SVC_CUTOVER_AT", "2026-08-02T13:45:06+02:00")

	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := time.Date(2026, 8, 2, 13, 45, 6, 0, time.FixedZone("", 2*60*60)); !cfg.CutoverAt.Equal(want) {
		t.Errorf("CutoverAt = %v, want %v", cfg.CutoverAt, want)
	}
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !cfg.Epoch.Equal(want) {
		t.Errorf("Epoch = %v, want %v", cfg.Epoch, want)
	}
}

func TestLoadRejectsNonRFC3339Time(t *testing.T) {
	type c struct {
		CutoverAt time.Time `env:"SVC_CUTOVER_AT"`
	}
	t.Setenv("SVC_CUTOVER_AT", "2026-08-02 13:45:06")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want an error for a non-RFC3339 timestamp, got nil")
	}
	if !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("error does not name the accepted form: %v", err)
	}
}

func TestLoadLeavesUntaggedTimeAlone(t *testing.T) {
	// time.Time is a struct; loadStruct must not walk into it looking for
	// tagged fields when it carries no env tag of its own.
	type c struct {
		Started time.Time
		Addr    string `env:"SVC_ADDR" default:":8080"`
	}
	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Started.IsZero() {
		t.Errorf("Started = %v, want the zero time", cfg.Started)
	}
}

// --- typed slices ------------------------------------------------------------

func TestLoadParsesTypedSlices(t *testing.T) {
	type c struct {
		Ports     []int           `env:"SVC_PORTS"`
		Weights   []float64       `env:"SVC_WEIGHTS"`
		Intervals []time.Duration `env:"SVC_INTERVALS" default:"1s,2m"`
	}
	t.Setenv("SVC_PORTS", "8080, 9090 ,7070")
	t.Setenv("SVC_WEIGHTS", "0.5,1.5")

	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := c{
		Ports:     []int{8080, 9090, 7070},
		Weights:   []float64{0.5, 1.5},
		Intervals: []time.Duration{time.Second, 2 * time.Minute},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadNamedStringSliceType(t *testing.T) {
	type hosts []string
	type c struct {
		Hosts hosts `env:"SVC_HOSTS"`
	}
	t.Setenv("SVC_HOSTS", "a.example,b.example")
	cfg, err := Load[c]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.Hosts, hosts{"a.example", "b.example"}) {
		t.Errorf("Hosts = %+v", cfg.Hosts)
	}
}

func TestLoadBadSliceElementNamesTheEnvVarAndTheIndex(t *testing.T) {
	type c struct {
		Ports []int `env:"SVC_PORTS"`
	}
	t.Setenv("SVC_PORTS", "8080,nope,7070")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "SVC_PORTS") || !strings.Contains(err.Error(), "element 1") {
		t.Errorf("error should name the variable and the element: %v", err)
	}
}

func TestLoadByteSliceIsUnsupported(t *testing.T) {
	// []byte is []uint8, which the numeric slice path would happily read as a
	// comma-separated list of small integers. An operator setting a []byte
	// field means raw bytes, so refuse rather than surprise them.
	type c struct {
		Blob []byte `env:"SVC_BLOB"`
	}
	t.Setenv("SVC_BLOB", "abc")
	_, err := Load[c]()
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("want ErrUnsupportedType for []byte, got %v", err)
	}
}

func TestLoadNestedSliceIsUnsupported(t *testing.T) {
	type c struct {
		Grid [][]string `env:"SVC_GRID"`
	}
	t.Setenv("SVC_GRID", "a,b")
	_, err := Load[c]()
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("want ErrUnsupportedType for [][]string, got %v", err)
	}
}

func TestLoadRedactsSecretSliceElementsInErrors(t *testing.T) {
	type c struct {
		Keys []int `env:"SVC_API_KEY_IDS"`
	}
	t.Setenv("SVC_API_KEY_IDS", "1,hunter2")
	_, err := Load[c]()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoed a value from a KEY-named variable: %v", err)
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
