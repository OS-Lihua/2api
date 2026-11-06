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
	"testing"
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
	mu  sync.Mutex
	now time.Time
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the fake clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// UseFake makes Now return a fake clock that starts at start, and restores
// system time when the test ends.
func UseFake(tb testing.TB, start time.Time) *Fake {
	tb.Helper()
	fake := &Fake{now: start}
	previous := fakeClock.Swap(fake)
	tb.Cleanup(func() { fakeClock.Store(previous) })
	return fake
}
