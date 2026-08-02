package outbound

import (
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func mustBudget(t *testing.T, cfg BudgetConfig) *WindowedBudget {
	t.Helper()
	b, err := NewWindowedBudget(cfg)
	if err != nil {
		t.Fatalf("NewWindowedBudget: %v", err)
	}
	return b
}

func TestWindowedBudgetValidatesConfig(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]BudgetConfig{
		"zero limit":            {Limit: 0},
		"negative limit":        {Limit: -1},
		"negative window":       {Limit: 10, Window: -time.Second},
		"calendar no window":    {Limit: 10, Calendar: true},
		"calendar sub-second":   {Limit: 10, Window: 500 * time.Millisecond, Calendar: true},
		"calendar not a second": {Limit: 10, Window: 1500 * time.Millisecond, Calendar: true},
	} {
		if _, err := NewWindowedBudget(cfg); err == nil {
			t.Errorf("%s: NewWindowedBudget succeeded, want an error", name)
		}
	}
}

func TestWindowedBudgetProcessLifetimeNeverRolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := mustBudget(t, BudgetConfig{Limit: 2}) // Window 0 == process lifetime
		for i := range 2 {
			if _, _, ok := b.Reserve(); !ok {
				t.Fatalf("reserve %d refused", i)
			}
		}
		time.Sleep(72 * time.Hour)
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("a process-lifetime budget rolled over; it must not")
		}
		st := b.Stats()
		if !st.WindowStart.IsZero() || !st.WindowEnd.IsZero() {
			t.Errorf("Stats window = [%v, %v), want zero times for a process-lifetime budget", st.WindowStart, st.WindowEnd)
		}
	})
}

func TestWindowedBudgetRollingWindowRefills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := mustBudget(t, BudgetConfig{Limit: 2, Window: time.Hour})
		start := b.Stats().WindowStart

		for range 2 {
			if _, _, ok := b.Reserve(); !ok {
				t.Fatal("reserve refused inside the limit")
			}
		}
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("third reserve allowed, want the window exhausted")
		}

		time.Sleep(59 * time.Minute)
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("budget refilled before the window elapsed")
		}

		time.Sleep(2 * time.Minute) // now 61 minutes in: one full window has passed
		used, remaining, ok := b.Reserve()
		if !ok {
			t.Fatal("budget did not refill after the window elapsed")
		}
		if used != 1 || remaining != 1 {
			t.Errorf("after roll: used=%d remaining=%d, want 1 and 1", used, remaining)
		}
		// The window is tumbling, aligned to the original start, not restarted
		// at the moment of the late call.
		if got, want := b.Stats().WindowStart, start.Add(time.Hour); !got.Equal(want) {
			t.Errorf("WindowStart = %v, want %v (tumbling from the original start)", got, want)
		}
	})
}

func TestWindowedBudgetSkipsWholeIdleWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := mustBudget(t, BudgetConfig{Limit: 1, Window: time.Hour})
		start := b.Stats().WindowStart
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("first reserve refused")
		}
		time.Sleep(10*time.Hour + 30*time.Minute)
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("reserve refused after ten idle windows")
		}
		if got, want := b.Stats().WindowStart, start.Add(10*time.Hour); !got.Equal(want) {
			t.Errorf("WindowStart = %v, want %v", got, want)
		}
	})
}

func TestWindowedBudgetCalendarDayRollsAtUTCMidnight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// This is the eBay case: 5,000 calls per UTC day, not per 24h of
		// process uptime. synctest's clock starts at 2000-01-01 00:00:00 UTC.
		b := mustBudget(t, BudgetConfig{Limit: 2, Window: 24 * time.Hour, Calendar: true})

		time.Sleep(23 * time.Hour) // 23:00 UTC
		for range 2 {
			if _, _, ok := b.Reserve(); !ok {
				t.Fatal("reserve refused inside the limit")
			}
		}
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("third reserve allowed on the same UTC day")
		}
		st := b.Stats()
		if h := st.WindowStart.UTC().Hour(); h != 0 {
			t.Errorf("WindowStart = %v, want a UTC midnight", st.WindowStart)
		}
		if got, want := st.WindowEnd.Sub(st.WindowStart), 24*time.Hour; got != want {
			t.Errorf("window length = %v, want %v", got, want)
		}

		time.Sleep(30 * time.Minute) // 23:30, still the same day
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("budget refilled before UTC midnight")
		}
		time.Sleep(45 * time.Minute) // 00:15 the next day
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("budget did not roll at UTC midnight")
		}
		if h := b.Stats().WindowStart.UTC().Hour(); h != 0 {
			t.Errorf("WindowStart = %v, want the new UTC midnight", b.Stats().WindowStart)
		}
	})
}

func TestWindowedBudgetResetStartsANewWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := mustBudget(t, BudgetConfig{Limit: 1, Window: time.Hour})
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("first reserve refused")
		}
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("second reserve allowed")
		}
		time.Sleep(10 * time.Minute)
		b.Reset()
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("Reset did not clear usage")
		}
		if got := b.Stats().WindowStart; !got.Equal(time.Now()) {
			t.Errorf("WindowStart = %v, want the moment of Reset (%v)", got, time.Now())
		}
	})
}

func TestWindowedBudgetObserveCapsBelowTheLocalLimit(t *testing.T) {
	t.Parallel()
	b := mustBudget(t, BudgetConfig{Limit: 1000})

	// The server is authoritative: it says two calls are left, so two calls
	// are left even though the local ledger thinks there are a thousand.
	b.Observe(2)
	if got := b.Stats().Remaining; got != 2 {
		t.Fatalf("Remaining = %d after Observe(2), want 2", got)
	}
	for i := range 2 {
		if _, _, ok := b.Reserve(); !ok {
			t.Fatalf("reserve %d refused", i)
		}
	}
	if _, _, ok := b.Reserve(); ok {
		t.Fatal("reserve allowed past the server-reported remaining count")
	}
	if got := b.Stats().Used; got != 2 {
		t.Errorf("Used = %d, want 2", got)
	}
}

func TestWindowedBudgetObserveDoesNotRaiseAboveTheLocalLimit(t *testing.T) {
	t.Parallel()
	b := mustBudget(t, BudgetConfig{Limit: 3})
	b.Observe(1_000_000) // a generous server does not widen our own ceiling
	if got := b.Stats().Remaining; got != 3 {
		t.Fatalf("Remaining = %d, want 3 (the local limit still binds)", got)
	}
	b.Observe(-5) // a negative report means "none left", not "unlimited"
	if got := b.Stats().Remaining; got != 0 {
		t.Fatalf("Remaining = %d after Observe(-5), want 0", got)
	}
}

func TestWindowedBudgetObserveIsForgottenOnRoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := mustBudget(t, BudgetConfig{Limit: 5, Window: time.Hour})
		b.Observe(0)
		if _, _, ok := b.Reserve(); ok {
			t.Fatal("reserve allowed with a server-reported zero remaining")
		}
		time.Sleep(time.Hour)
		if _, _, ok := b.Reserve(); !ok {
			t.Fatal("a stale server-reported count survived the window roll")
		}
	})
}

func TestWindowedBudgetIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	const limit = 500
	b := mustBudget(t, BudgetConfig{Limit: limit})
	granted := make(chan bool, 8*limit)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range limit {
				_, _, ok := b.Reserve()
				granted <- ok
			}
		}()
	}
	for range 8 {
		<-done
	}
	close(granted)
	n := 0
	for ok := range granted {
		if ok {
			n++
		}
	}
	if n != limit {
		t.Fatalf("granted %d reservations, want exactly %d", n, limit)
	}
	if got := b.Stats().Used; got != limit {
		t.Fatalf("Used = %d, want %d", got, limit)
	}
}

// --- integration with Client -------------------------------------------------

func TestClientRejectsBothBudgetForms(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Product: "kittest", ContactURL: "https://example.invalid/bots",
		Timeout: time.Second, Unlimited: true,
		CallBudget: 10,
		Budget:     mustBudget(t, BudgetConfig{Limit: 10}),
	})
	if err == nil {
		t.Fatal("New accepted both CallBudget and Budget, want an error")
	}
}

func TestClientWindowedBudgetRefills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := okTransport()
		b := mustBudget(t, BudgetConfig{Limit: 1, Window: time.Hour})
		c := mustClient(t, Config{Unlimited: true, Budget: b, transport: tr})

		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		closeBody(t, resp)

		//nolint:bodyclose // the budget refuses the call; there is no response
		if _, err := get(t, c, "https://example.com/"); !errors.Is(err, ErrCallBudgetExhausted) {
			t.Fatalf("second call err = %v, want ErrCallBudgetExhausted", err)
		}

		time.Sleep(time.Hour)
		resp, err = get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("call after the window rolled: %v", err)
		}
		closeBody(t, resp)
		if tr.count() != 2 {
			t.Fatalf("requests = %d, want 2", tr.count())
		}
	})
}

func TestClientExposesItsBudget(t *testing.T) {
	t.Parallel()
	b := mustBudget(t, BudgetConfig{Limit: 4})
	c := mustClient(t, Config{Unlimited: true, Budget: b, transport: okTransport()})
	if c.Budget() != Budget(b) {
		t.Error("Budget() did not return the configured budget")
	}
	if got := c.BudgetRemaining(); got != 4 {
		t.Errorf("BudgetRemaining() = %d, want 4", got)
	}

	plain := mustClient(t, Config{Unlimited: true, transport: okTransport()})
	if plain.Budget() != nil {
		t.Error("Budget() is non-nil on an unmetered client")
	}
	if got := plain.BudgetRemaining(); got != -1 {
		t.Errorf("BudgetRemaining() = %d on an unmetered client, want -1", got)
	}
}

// CallBudget keeps working exactly as it did in v0.1.2: a process-lifetime
// ceiling that never refills.
func TestLegacyCallBudgetIsProcessLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := okTransport()
		c := mustClient(t, Config{Unlimited: true, CallBudget: 1, transport: tr})

		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
		closeBody(t, resp)

		time.Sleep(30 * 24 * time.Hour)
		//nolint:bodyclose // the budget refuses the call; there is no response
		if _, err := get(t, c, "https://example.com/"); !errors.Is(err, ErrCallBudgetExhausted) {
			t.Fatalf("err = %v, want ErrCallBudgetExhausted a month later too", err)
		}
	})
}

// The budget counts ATTEMPTS, not logical calls: a retried request consumes
// what the remote actually saw. nagus's eBay integration depends on this, and
// on being able to set MaxAttempts: 1 so a budgeted API is never retried.
func TestBudgetChargesEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			return stubResponse(req, http.StatusTooManyRequests, map[string]string{"Retry-After": "1"}), nil
		}}
		b := mustBudget(t, BudgetConfig{Limit: 10})
		c := mustClient(t, Config{Unlimited: true, Budget: b, MaxAttempts: 3, transport: tr})

		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		closeBody(t, resp)
		if got := b.Stats().Used; got != 3 {
			t.Fatalf("budget Used = %d, want 3 (one per attempt)", got)
		}
	})
}
