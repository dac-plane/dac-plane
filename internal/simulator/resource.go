package simulator

import (
	"fmt"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ResourceAction is the effect kind a stateful resource applies when one of
// its tickets finishes.
const ResourceAction EffectKind = "resource_action"

// Resource is one stateful tool a scripted test drives directly, without a
// provider port: it holds a state token, admits at most one action at a time
// and applies that action's effect when the ticket finishes. The token
// rotates on every change, so an action planned on an older observation is
// refused the way the real contracts refuse it (contracts.CheckPrecondition)
// and a second action while one is in flight is refused the same way
// (contracts.RequireSerial): the ledger proves the refused action applied
// nothing.
type Resource struct {
	name   string
	load   uint64
	token  identity.Digest
	open   map[ids.OperationID]contracts.ActionPrecondition
	ledger *Ledger
	mu     sync.Mutex
}

// NewResource returns a resource with no actions in flight. name is the
// reference that actions and tests address it by.
func NewResource(name string) *Resource {
	resource := &Resource{
		name:   name,
		open:   map[ids.OperationID]contracts.ActionPrecondition{},
		ledger: newLedger(),
	}
	resource.token = resource.rotate()
	return resource
}

// rotate advances the resource's load and returns the token of its current
// state. The caller holds the mutex or owns the resource.
func (r *Resource) rotate() identity.Digest {
	r.load++
	return identity.DigestOf([]byte(fmt.Sprintf("%s:%d", r.name, r.load)))
}

// Token returns the digest of the resource's current state. An action planned
// on it stays legal until the next change.
func (r *Resource) Token() identity.Digest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token
}

// Change rewrites the resource the way an external write would: the state and
// its token are no longer what a planned action observed, so that action's
// precondition is stale until the caller observes again.
func (r *Resource) Change() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.token = r.rotate()
}

// Begin admits one action against the resource's current observation. It
// refuses a precondition that is stale or names another resource and refuses
// while any action on the resource is in flight; an admitted action stays
// open until its ticket finishes.
func (r *Resource) Begin(precondition contracts.ActionPrecondition) (*Ticket, *contracts.Failure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if failure := contracts.CheckPrecondition(precondition, r.name, r.token); failure != nil {
		return nil, failure
	}
	if _, open := r.open[precondition.Operation]; open {
		return nil, &contracts.Failure{Code: contracts.FailureResourceBusy, Message: "this operation already has an action in flight"}
	}
	inFlight := make([]contracts.ActionPrecondition, 0, len(r.open))
	for _, action := range r.open {
		inFlight = append(inFlight, action)
	}
	if failure := contracts.RequireSerial(inFlight, precondition); failure != nil {
		return nil, failure
	}
	r.open[precondition.Operation] = precondition
	return &Ticket{resource: r, precondition: precondition}, nil
}

// Ledger returns the resource's record of applied effects.
func (r *Resource) Ledger() *Ledger {
	return r.ledger
}

// Ticket is one admitted action on a resource. Finishing it applies the
// action's effect once, rotates the resource's token and frees the resource
// for the next action. A ticket finishes once: finishing it again is a
// scenario bug and fails loudly.
type Ticket struct {
	resource     *Resource
	precondition contracts.ActionPrecondition
	output       identity.Digest
}

// Abort frees a reserved resource when execution stops before its effect.
func (t *Ticket) Abort() {
	t.resource.mu.Lock()
	defer t.resource.mu.Unlock()
	delete(t.resource.open, t.precondition.Operation)
}

// Finish applies the action's effect exactly once and closes the ticket.
func (t *Ticket) Finish() error {
	resource := t.resource
	resource.mu.Lock()
	defer resource.mu.Unlock()
	if _, open := resource.open[t.precondition.Operation]; !open {
		return fmt.Errorf("%w: ticket for %s is not open", ErrInvalidScenario, t.precondition.Operation)
	}
	delete(resource.open, t.precondition.Operation)
	if failure := contracts.CheckPrecondition(t.precondition, resource.name, resource.token); failure != nil {
		return failure
	}
	resource.token = resource.rotate()
	t.output = resource.token
	resource.ledger.recordEffect(Effect{
		Operation: t.precondition.Operation,
		Kind:      ResourceAction,
		Detail:    resource.name,
	})
	return nil
}

// OutputDigest is the state produced by this ticket, captured atomically
// with its effect rather than a later observation of the shared resource.
func (t *Ticket) OutputDigest() identity.Digest {
	t.resource.mu.Lock()
	defer t.resource.mu.Unlock()
	return t.output
}
