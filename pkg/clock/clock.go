// Package clock is the gateway's single source of the current time.
//
// Production code calls Now, which returns system time. A test calls UseFake
// to replace it with a fake clock that it sets and advances directly, so time
// passes without real waiting. The fake clock is process-wide: a test that
// calls UseFake must not run in parallel with other tests that read Now.
package clock

import (
	"sync"
	"sync/atomic"
	"time"
)

var fakeClock atomic.Pointer[Fake]

// Now returns the current time: system time, or the fake clock's time while a
// test has one installed.
func Now() time.Time {
	if fake := fakeClock.Load(); fake != nil {
		return fake.Now()
	}
	return time.Now()
}

// Fake is a clock whose time changes only when a test sets or advances it.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.Set(f.Now().Add(d))
}

// Set moves the fake clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t
	var ready []func()
	for _, timer := range f.timers {
		if !timer.stopped && !t.Before(timer.at) {
			timer.stopped = true
			ready = append(ready, timer.fn)
		}
	}
	f.mu.Unlock()
	for _, fn := range ready {
		fn()
	}
}

// TB is the part of testing.TB that UseFake needs. It keeps the testing
// package out of production binaries that import clock.
type fakeTimer struct {
	at      time.Time
	fn      func()
	stopped bool
}

// AfterFunc runs fn after d on the active clock. Fake callbacks run outside its
// lock when Advance or Set reaches the deadline, without a real-time wait.
func AfterFunc(d time.Duration, fn func()) func() bool {
	if fake := fakeClock.Load(); fake != nil {
		fake.mu.Lock()
		timer := &fakeTimer{at: fake.now.Add(d), fn: fn}
		fake.timers = append(fake.timers, timer)
		fake.mu.Unlock()
		return func() bool {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			active := !timer.stopped
			timer.stopped = true
			return active
		}
	}
	return time.AfterFunc(d, fn).Stop
}

type TB interface {
	Helper()
	Cleanup(func())
}

// UseFake makes Now return a fake clock that starts at start, and restores
// system time when the test ends.
func UseFake(tb TB, start time.Time) *Fake {
	tb.Helper()
	fake := &Fake{now: start}
	previous := fakeClock.Swap(fake)
	tb.Cleanup(func() { fakeClock.Store(previous) })
	return fake
}
