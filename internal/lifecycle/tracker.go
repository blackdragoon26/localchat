package lifecycle

import (
	"sync/atomic"
	"time"
)

// Tracker coordinates on-demand idle shutdown while protecting active
// uploads, downloads, and outbound peer transfers.
type Tracker struct {
	timeout     time.Duration
	now         func() time.Time
	lastUINanos atomic.Int64
	inFlight    atomic.Int64
	disableIdle bool
}

// NewTracker creates a Tracker initialized with the current time as last UI activity.
func NewTracker(timeout time.Duration, disableIdle bool) *Tracker {
	return NewTrackerWithClock(timeout, disableIdle, time.Now)
}

// NewTrackerWithClock creates a Tracker with an injectable clock for deterministic tests.
func NewTrackerWithClock(timeout time.Duration, disableIdle bool, clock func() time.Time) *Tracker {
	if clock == nil {
		clock = time.Now
	}
	t := &Tracker{
		timeout:     timeout,
		now:         clock,
		disableIdle: disableIdle,
	}
	t.TouchUI()
	return t
}

// TouchUI records local browser UI activity (API calls and hidden-tab heartbeats).
// Passive peer manifest probes must not call TouchUI.
func (t *Tracker) TouchUI() {
	t.lastUINanos.Store(t.now().UnixNano())
}

// BeginTransfer marks an active upload, download, or peer sync transfer.
func (t *Tracker) BeginTransfer() {
	t.inFlight.Add(1)
}

// EndTransfer marks completion of an active transfer.
func (t *Tracker) EndTransfer() {
	if t.inFlight.Add(-1) < 0 {
		t.inFlight.Store(0)
	}
}

// InFlight returns the number of currently active transfers.
func (t *Tracker) InFlight() int64 {
	return t.inFlight.Load()
}

// UIActiveWithin reports whether local UI activity occurred within d.
func (t *Tracker) UIActiveWithin(d time.Duration) bool {
	last := time.Unix(0, t.lastUINanos.Load())
	return t.now().Sub(last) <= d
}

// ShouldExit returns true when idle shutdown is enabled, no transfers are in flight,
// and local UI activity has exceeded the configured idle timeout.
func (t *Tracker) ShouldExit() bool {
	if t.disableIdle || t.timeout <= 0 {
		return false
	}
	if t.inFlight.Load() > 0 {
		return false
	}
	last := time.Unix(0, t.lastUINanos.Load())
	return t.now().Sub(last) >= t.timeout
}
