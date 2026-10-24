package adapterkit

import (
	"math/rand/v2"
	"time"
)

// backoff produces reconnect delays with exponential growth and full jitter:
// each delay is uniform in [0, min(max, base*2^n)), which spreads a fleet of
// adapters reconnecting after the same outage.
type backoff struct {
	base, max time.Duration
	attempt   int
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{base: base, max: max}
}

func (b *backoff) next() time.Duration {
	ceiling := b.max
	if b.attempt < 30 {
		if grown := b.base << b.attempt; grown > 0 && grown < ceiling {
			ceiling = grown
		}
	}
	b.attempt++
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

func (b *backoff) reset() {
	b.attempt = 0
}
