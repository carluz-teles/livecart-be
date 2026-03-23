package clock

import "time"

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func New() Clock {
	return &realClock{}
}

func (c *realClock) Now() time.Time {
	return time.Now().UTC()
}
