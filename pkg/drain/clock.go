package drain

import "time"

type clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func defaultClock(c clock) clock {
	if c != nil {
		return c
	}
	return realClock{}
}
