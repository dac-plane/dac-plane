package simulator

import (
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Spender is the principal's side of the spend ceremony: a stand-in that
// answers a challenge with an assertion. The knobs make it misbehave the way
// real tooling does: Drift perturbs the terms it answers, Expired answers at
// the expiry instant, and Played reuses the previous assertion for the next
// challenge. The machine, not the spender, decides what those answers mean.
type Spender struct {
	Tenant    ids.TenantID
	Principal ids.PrincipalID
	Expired   bool
	Drift     bool
	Played    bool

	last lifecycle.SpendAssertion
}

// Assert answers one challenge for this principal at at. The proof digest is
// derived from the challenge and nonce, the way a credential ceremony's output
// would be; the proof validates as a digest, never as a credential.
func (s *Spender) Assert(challenge lifecycle.SpendChallenge, at time.Time) (lifecycle.SpendAssertion, error) {
	if err := challenge.Validate(); err != nil {
		return lifecycle.SpendAssertion{}, err
	}
	if challenge.Terms.Tenant != s.Tenant || challenge.Terms.Principal != s.Principal {
		return lifecycle.SpendAssertion{}, fmt.Errorf("%w: challenge is not this principal's", ErrInvalidScenario)
	}
	if s.Played && !s.last.AssertedAt.IsZero() {
		return s.last, nil
	}
	if s.Expired {
		at = challenge.ExpiresAt
	}
	terms := challenge.Terms
	if s.Drift {
		terms.Amount++
	}
	assertion := lifecycle.SpendAssertion{
		Challenge:  challenge.ID,
		Terms:      terms,
		Proof:      identity.DigestOf([]byte(challenge.ID.String() + ":" + string(terms.Nonce))),
		AssertedAt: at,
	}
	s.last = assertion
	return assertion, nil
}
