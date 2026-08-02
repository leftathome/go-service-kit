package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/go-service-kit/httpapi"
)

// The invariant that survived making the budgets configurable: zero means the
// DEFAULT, never "no timeout". net/http reads a zero timeout as unbounded, so
// this is the difference between a knob and a footgun.
func TestZeroTimeoutsMeanDefaultsNotUnbounded(t *testing.T) {
	t.Parallel()

	servers := map[string]*http.Server{
		"public": httpapi.New(httpapi.Options{Timeouts: httpapi.Timeouts{}}).Server,
		"admin":  httpapi.NewAdmin(httpapi.AdminOptions{Timeouts: httpapi.Timeouts{}}).Server,
	}
	for name, srv := range servers {
		if srv.ReadHeaderTimeout != httpapi.DefaultReadHeaderTimeout {
			t.Errorf("%s: ReadHeaderTimeout = %v, want the default %v", name, srv.ReadHeaderTimeout, httpapi.DefaultReadHeaderTimeout)
		}
		if srv.ReadTimeout != httpapi.DefaultReadTimeout {
			t.Errorf("%s: ReadTimeout = %v, want the default %v", name, srv.ReadTimeout, httpapi.DefaultReadTimeout)
		}
		if srv.WriteTimeout != httpapi.DefaultWriteTimeout {
			t.Errorf("%s: WriteTimeout = %v, want the default %v", name, srv.WriteTimeout, httpapi.DefaultWriteTimeout)
		}
		if srv.IdleTimeout != httpapi.DefaultIdleTimeout {
			t.Errorf("%s: IdleTimeout = %v, want the default %v", name, srv.IdleTimeout, httpapi.DefaultIdleTimeout)
		}
		if srv.MaxHeaderBytes != httpapi.DefaultMaxHeaderBytes {
			t.Errorf("%s: MaxHeaderBytes = %d, want the default %d", name, srv.MaxHeaderBytes, httpapi.DefaultMaxHeaderBytes)
		}
	}
}

// nagus's /watches is a fan-out READ that can exceed 30s. Exceeding
// WriteTimeout truncates the body mid-JSON rather than returning an error, so
// the budget has to be raisable.
func TestTimeoutsAreConfigurablePerListener(t *testing.T) {
	t.Parallel()

	want := httpapi.Timeouts{
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       90 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	api := httpapi.New(httpapi.Options{Timeouts: want}).Server
	if api.WriteTimeout != want.WriteTimeout {
		t.Errorf("public WriteTimeout = %v, want %v", api.WriteTimeout, want.WriteTimeout)
	}
	if api.ReadTimeout != want.ReadTimeout {
		t.Errorf("public ReadTimeout = %v, want %v", api.ReadTimeout, want.ReadTimeout)
	}
	if api.ReadHeaderTimeout != want.ReadHeaderTimeout {
		t.Errorf("public ReadHeaderTimeout = %v, want %v", api.ReadHeaderTimeout, want.ReadHeaderTimeout)
	}
	if api.IdleTimeout != want.IdleTimeout {
		t.Errorf("public IdleTimeout = %v, want %v", api.IdleTimeout, want.IdleTimeout)
	}
	if api.MaxHeaderBytes != want.MaxHeaderBytes {
		t.Errorf("public MaxHeaderBytes = %d, want %d", api.MaxHeaderBytes, want.MaxHeaderBytes)
	}

	// The two listeners are configured independently: raising the API's write
	// budget must not drag the admin listener along.
	admin := httpapi.NewAdmin(httpapi.AdminOptions{}).Server
	if admin.WriteTimeout != httpapi.DefaultWriteTimeout {
		t.Errorf("admin WriteTimeout = %v, want the default %v", admin.WriteTimeout, httpapi.DefaultWriteTimeout)
	}
}

// A partially specified Timeouts fills the rest from the defaults, so raising
// one budget cannot accidentally zero the others.
func TestPartialTimeoutsKeepTheOtherDefaults(t *testing.T) {
	t.Parallel()

	srv := httpapi.New(httpapi.Options{
		Timeouts: httpapi.Timeouts{WriteTimeout: 90 * time.Second},
	}).Server

	if srv.WriteTimeout != 90*time.Second {
		t.Errorf("WriteTimeout = %v, want 90s", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != httpapi.DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want the default %v (gosec G112)", srv.ReadHeaderTimeout, httpapi.DefaultReadHeaderTimeout)
	}
	if srv.ReadTimeout != httpapi.DefaultReadTimeout {
		t.Errorf("ReadTimeout = %v, want the default %v", srv.ReadTimeout, httpapi.DefaultReadTimeout)
	}
}

// A negative duration is net/http's spelling of "no timeout". Accepting it
// would make a timeout-less server constructable again, so it is rejected --
// loudly, at construction, not silently replaced with a default the caller did
// not ask for.
func TestNegativeTimeoutIsRejected(t *testing.T) {
	t.Parallel()

	cases := map[string]httpapi.Timeouts{
		"ReadHeaderTimeout": {ReadHeaderTimeout: -1},
		"ReadTimeout":       {ReadTimeout: -1},
		"WriteTimeout":      {WriteTimeout: -time.Second},
		"IdleTimeout":       {IdleTimeout: -time.Hour},
		"MaxHeaderBytes":    {MaxHeaderBytes: -1},
	}
	for field, to := range cases {
		err := to.Validate()
		if err == nil {
			t.Errorf("Timeouts{%s: negative}.Validate() = nil, want an error", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Validate error for %s does not name the field: %v", field, err)
		}

		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New with a negative %s did not panic; a timeout-less server must be unconstructable", field)
				}
			}()
			httpapi.New(httpapi.Options{Timeouts: to})
		}()

		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewAdmin with a negative %s did not panic", field)
				}
			}()
			httpapi.NewAdmin(httpapi.AdminOptions{Timeouts: to})
		}()
	}

	if err := (httpapi.Timeouts{}).Validate(); err != nil {
		t.Errorf("the zero Timeouts must validate, got %v", err)
	}
}

func TestTimeoutsWithDefaultsIsPure(t *testing.T) {
	t.Parallel()

	in := httpapi.Timeouts{WriteTimeout: 90 * time.Second}
	got := in.WithDefaults()

	if in.ReadTimeout != 0 {
		t.Errorf("WithDefaults mutated the receiver: ReadTimeout = %v", in.ReadTimeout)
	}
	if got.WriteTimeout != 90*time.Second {
		t.Errorf("WithDefaults changed an explicit value: WriteTimeout = %v", got.WriteTimeout)
	}
	if got != got.WithDefaults() {
		t.Error("WithDefaults is not idempotent")
	}
}
