package outbound

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Budget meters outbound attempts against a quota. Implementations must be safe
// for concurrent use.
//
// The unit is the ATTEMPT, not the logical call: a request retried twice costs
// three units, because three is what the remote saw and what its quota counted.
// A service on a metered API should therefore consider MaxAttempts: 1 -- a
// budgeted endpoint is the wrong place for silent retries.
//
// The interface exists because a real quota is not a process-lifetime counter.
// It has a window (eBay Browse: 5,000 calls per UTC DAY, not per 24h of pod
// uptime), it is often reported by the server (X-RateLimit-Remaining), and an
// operator sometimes needs to clear it by hand. [WindowedBudget] implements all
// of that; a service with a stranger quota -- one shared across pods through
// Redis, say -- implements this interface instead and hands it to
// [Config.Budget].
type Budget interface {
	// Reserve charges one attempt. It reports the attempts charged in the
	// current window, how many remain, and whether the call may proceed.
	// When ok is false NOTHING is charged.
	Reserve() (used, remaining int64, ok bool)

	// Observe records a remaining count the server reported. The server is
	// authoritative downward: a figure below the local headroom caps it for
	// the rest of the window. It is not authoritative upward -- a generous
	// report never widens a ceiling the service chose for itself.
	Observe(remaining int64)

	// Reset clears usage and starts a new window now. This is the manual
	// override: a keyset was replaced, or a quota was raised mid-window.
	Reset()

	// Stats is a point-in-time snapshot for metrics and debug surfaces.
	Stats() BudgetStats
}

// BudgetStats is a point-in-time view of a Budget.
type BudgetStats struct {
	// Limit is the ceiling for one window.
	Limit int64
	// Used is what has been charged in the current window.
	Used int64
	// Remaining is the smaller of the local headroom and any server-reported
	// remaining count. It is never negative.
	Remaining int64
	// WindowStart and WindowEnd bound the current window. Both are the zero
	// time for a process-lifetime budget, which has no window to report.
	WindowStart time.Time
	WindowEnd   time.Time
}

// BudgetConfig describes a [WindowedBudget].
type BudgetConfig struct {
	// Limit is the number of attempts permitted per window. Required, positive.
	Limit int64

	// Window is the length of one window. Zero means process lifetime: the
	// budget never refills, which is what Config.CallBudget has always meant.
	Window time.Duration

	// Calendar aligns the window to UTC boundaries derived from the Unix
	// epoch, rather than starting it when the process did. With
	// Window: 24*time.Hour that is exactly "per UTC day" -- the shape real
	// daily quotas actually have, and the reason a process-lifetime counter
	// cannot express them. Requires a whole number of seconds.
	Calendar bool

	// now is a test seam. Production code uses time.Now; time-dependent
	// behaviour is testable from outside the package under testing/synctest,
	// which fakes time.Now itself.
	now func() time.Time
}

// WindowedBudget is a quota ledger with an optional window, a server-reported
// override, and a manual reset. It is safe for concurrent use.
type WindowedBudget struct {
	limit      int64
	window     time.Duration
	windowSecs int64 // window in whole seconds; calendar mode only
	calendar   bool
	now        func() time.Time

	mu       sync.Mutex
	used     int64
	start    time.Time // current window start; zero when unwindowed
	index    int64     // calendar window index, so a roll is one comparison
	reported int64     // last server-reported remaining; -1 when unknown
}

// NewWindowedBudget validates cfg and builds a budget.
func NewWindowedBudget(cfg BudgetConfig) (*WindowedBudget, error) {
	var errs []error
	if cfg.Limit <= 0 {
		errs = append(errs, errors.New("outbound: BudgetConfig.Limit must be positive"))
	}
	if cfg.Window < 0 {
		errs = append(errs, errors.New("outbound: BudgetConfig.Window must not be negative"))
	}
	if cfg.Calendar {
		switch {
		case cfg.Window <= 0:
			errs = append(errs, errors.New("outbound: BudgetConfig.Calendar needs a positive Window to align"))
		case cfg.Window%time.Second != 0:
			errs = append(errs, errors.New("outbound: BudgetConfig.Calendar requires a whole number of seconds"))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	now := cfg.now
	if now == nil {
		now = time.Now
	}
	b := &WindowedBudget{
		limit:    cfg.Limit,
		window:   cfg.Window,
		calendar: cfg.Calendar,
		now:      now,
		reported: -1,
	}
	if cfg.Calendar {
		b.windowSecs = int64(cfg.Window / time.Second)
	}
	b.mu.Lock()
	b.startWindowLocked()
	b.mu.Unlock()
	return b, nil
}

// Limit reports the per-window ceiling.
func (b *WindowedBudget) Limit() int64 { return b.limit }

// Window reports the window length; zero means process lifetime.
func (b *WindowedBudget) Window() time.Duration { return b.window }

// Reserve implements [Budget].
func (b *WindowedBudget) Reserve() (used, remaining int64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if b.remainingLocked() <= 0 {
		return b.used, 0, false
	}
	b.used++
	if b.reported > 0 {
		// Keep the server's figure moving with our own spend; the next report
		// overwrites it anyway, and until then it must not go stale high.
		b.reported--
	}
	return b.used, b.remainingLocked(), true
}

// Observe implements [Budget].
func (b *WindowedBudget) Observe(remaining int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if remaining < 0 {
		// A negative report is "none left", not "unlimited". Reading it the
		// other way would turn a malformed header into a quota override.
		remaining = 0
	}
	b.reported = remaining
}

// Reset implements [Budget].
func (b *WindowedBudget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used = 0
	b.reported = -1
	b.startWindowLocked()
}

// Stats implements [Budget].
func (b *WindowedBudget) Stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	st := BudgetStats{
		Limit:     b.limit,
		Used:      b.used,
		Remaining: b.remainingLocked(),
	}
	if b.window > 0 {
		st.WindowStart = b.start
		st.WindowEnd = b.start.Add(b.window)
	}
	return st
}

// remainingLocked is the local headroom, capped by any server report.
func (b *WindowedBudget) remainingLocked() int64 {
	rem := b.limit - b.used
	if rem < 0 {
		rem = 0
	}
	if b.reported >= 0 && b.reported < rem {
		rem = b.reported
	}
	return rem
}

// startWindowLocked anchors the current window on the clock's present value.
func (b *WindowedBudget) startWindowLocked() {
	if b.window <= 0 {
		return
	}
	now := b.now()
	if b.calendar {
		b.index = now.UTC().Unix() / b.windowSecs
		b.start = time.Unix(b.index*b.windowSecs, 0).UTC()
		return
	}
	b.start = now
}

// rollLocked refills the ledger when the window has advanced.
//
// A rolling window TUMBLES from its original start rather than restarting at
// the moment of the first late call: a poller that wakes every 61 minutes on an
// hourly budget must not drift its window forward by a minute each pass until
// it lines up with something else entirely.
func (b *WindowedBudget) rollLocked() {
	if b.window <= 0 {
		return
	}
	now := b.now()
	if b.calendar {
		idx := now.UTC().Unix() / b.windowSecs
		if idx == b.index {
			return
		}
		b.index = idx
		b.start = time.Unix(idx*b.windowSecs, 0).UTC()
		b.used = 0
		b.reported = -1
		return
	}
	elapsed := now.Sub(b.start)
	if elapsed < b.window {
		return
	}
	b.start = b.start.Add(time.Duration(elapsed/b.window) * b.window)
	b.used = 0
	b.reported = -1
}

// budgetFor resolves the two spellings of a quota into one Budget. The legacy
// Config.CallBudget is exactly a process-lifetime WindowedBudget.
func budgetFor(cfg Config) (Budget, error) {
	if cfg.Budget != nil {
		if cfg.CallBudget > 0 {
			return nil, errors.New("outbound: Config.CallBudget and Config.Budget are mutually exclusive; CallBudget is the process-lifetime special case of Budget")
		}
		return cfg.Budget, nil
	}
	if cfg.CallBudget <= 0 {
		return nil, nil //nolint:nilnil // no budget configured is a valid, unmetered client
	}
	b, err := NewWindowedBudget(BudgetConfig{Limit: cfg.CallBudget})
	if err != nil {
		return nil, fmt.Errorf("outbound: Config.CallBudget: %w", err)
	}
	return b, nil
}
