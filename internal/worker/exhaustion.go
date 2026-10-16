package worker

import (
	"math/rand/v2"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
)

// ExhaustionPolicy bounds dispatch retries. The bound travels as worker
// config; the per-row facts (attempts, not-before, last error) live on the
// outbox row.
type ExhaustionPolicy struct {
	// MaxAttempts caps sends per outbox row before it is exhausted.
	MaxAttempts int
	// BaseBackoff is the unit delay; attempt n waits BaseBackoff * 2^n
	// plus jitter, capped by MaxBackoff.
	BaseBackoff time.Duration
	// MaxBackoff caps any single retry delay.
	MaxBackoff time.Duration
}

// DefaultExhaustionPolicy is the SPEC §22 default: up to 3 automatic
// attempts for safe operations, then an actionable (inspectable) failure.
func DefaultExhaustionPolicy() ExhaustionPolicy {
	return ExhaustionPolicy{MaxAttempts: 3, BaseBackoff: 5 * time.Second, MaxBackoff: 5 * time.Minute}
}

// Classify maps a send failure to retry-or-give-up. It returns the delay
// before the row is due again, or exhausted=true when the row must stop
// polling. Codes that can never succeed on retry (auth, arguments, policy,
// cancellation, stale preconditions) exhaust immediately; transient codes
// back off until the budget runs out.
func (p ExhaustionPolicy) Classify(attempts int, code contracts.FailureCode) (retryIn time.Duration, exhausted bool) {
	if attempts >= p.MaxAttempts {
		return 0, true
	}
	switch code {
	case contracts.FailureProtocolMismatch,
		contracts.FailureUnsupportedCapability,
		contracts.FailureMalformedFrame,
		contracts.FailureUnknownOperation,
		contracts.FailureInvalidArguments,
		contracts.FailureUnauthenticated,
		contracts.FailureUnauthorized,
		contracts.FailureCancelled,
		contracts.FailureStalePrecondition:
		return 0, true
	case contracts.FailureDeadlineExceeded,
		contracts.FailureTransportUnavailable,
		contracts.FailureProviderFailed,
		contracts.FailureResourceBusy:
		return p.backoff(attempts), false
	default:
		return 0, true
	}
}

// backoff is exponential in attempts with up to 25% jitter, capped at
// MaxBackoff. Jitter is random, so table tests assert ranges.
func (p ExhaustionPolicy) backoff(attempts int) time.Duration {
	delay := p.BaseBackoff << attempts
	if delay <= 0 || delay > p.MaxBackoff {
		delay = p.MaxBackoff
	}
	return delay + time.Duration(rand.Float64()*0.25*float64(delay))
}
