package simio

import "time"

// Clock abstracts time operations for deterministic simulation.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	Sleep(d time.Duration)
}

// RealClock delegates to the standard time package.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (RealClock) Sleep(d time.Duration)                  { time.Sleep(d) }
