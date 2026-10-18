// Package simulator provides deterministic provider simulators behind the
// adapter contracts: a test scripts an explicit scenario, paces the provider
// at every external-effect boundary and inspects an effect ledger.
//
// Nothing here is production wiring. A simulator answers only scripted
// operations (an unscripted one fails loudly, never inventing a success),
// applies an operation's effect at most once across redeliveries, and emits
// only the frames and events its scenario carries; the consumer still
// validates, binds and ingests them.
package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Errors a simulator returns when its config, its scenario or its caller is
// wrong. They are never a scripted provider outcome: scenario outcomes travel
// as replies, failures and stream frames.
var (
	ErrInvalidConfig    = errors.New("invalid simulator config")
	ErrInvalidScenario  = errors.New("invalid simulator scenario")
	ErrUnscripted       = errors.New("unscripted operation")
	ErrForeignRequest   = errors.New("request is not this simulator's")
	ErrMalformedRequest = errors.New("malformed request")
)

// DefaultHello returns the lifecycle hello a simulator advertises when its
// config names no other: version 1 with no capabilities.
func DefaultHello() contracts.Lifecycle {
	return contracts.Lifecycle{Phase: contracts.PhaseHello, Versions: []uint32{1}}
}

// Config is what every simulator needs: the tenant and provider identity it
// acts as, the lifecycle hello it advertises and the control that paces it.
type Config struct {
	Tenant   ids.TenantID
	Provider ids.ProviderID
	Hello    contracts.Lifecycle
	Control  *Control
}

func (c Config) withDefaults() Config {
	if c.Control == nil {
		c.Control = NewControl()
	}
	if c.Hello.Phase == "" {
		c.Hello = DefaultHello()
	}
	return c
}

func (c Config) validate() error {
	if c.Tenant.IsZero() || c.Provider.IsZero() {
		return fmt.Errorf("%w: tenant and provider are required", ErrInvalidConfig)
	}
	if c.Hello.Phase != contracts.PhaseHello {
		return fmt.Errorf("%w: hello must advertise a hello phase", ErrInvalidConfig)
	}
	if err := c.Hello.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return nil
}

// Boundary is a point in handling one operation at which a test can pause a
// simulator before it continues.
type Boundary string

const (
	// BeforeEffect is after a request is validated and recorded, before its
	// external effect is applied. A call abandoned here leaves no effect.
	BeforeEffect Boundary = "before_effect"
	// AfterEffect is after the effect is applied and recorded, before the
	// reply is returned. A call abandoned here leaves an effect a redelivery
	// must not repeat.
	AfterEffect Boundary = "after_effect"
	// BeforeFrame is before a stream frame is handed to its subscriber.
	BeforeFrame Boundary = "before_frame"
)

// Control pauses simulators at named boundaries for named operations. A pause
// is registered before the operation runs and is consumed by the first
// delivery that reaches the boundary; unregistered boundaries never block, so
// tests that do not pace run straight through. A paused call still observes
// its context, so a test can abandon it and leave the effect half-done,
// exactly as a crash would.
type Control struct {
	mu     sync.Mutex
	pauses map[pauseKey]*Pause
}

type pauseKey struct {
	operation ids.OperationID
	boundary  Boundary
}

// NewControl returns a control with no pauses registered.
func NewControl() *Control {
	return &Control{pauses: map[pauseKey]*Pause{}}
}

// Pause registers a pause at boundary for the next delivery of operation and
// returns the handle that signals and releases it. Registering another pause
// for the same operation and boundary replaces the first.
func (c *Control) Pause(operation ids.OperationID, boundary Boundary) *Pause {
	pause := &Pause{reached: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauses[pauseKey{operation: operation, boundary: boundary}] = pause
	return pause
}

// reach announces and blocks at the boundary when a pause is registered.
func (c *Control) reach(ctx context.Context, operation ids.OperationID, boundary Boundary) error {
	key := pauseKey{operation: operation, boundary: boundary}
	c.mu.Lock()
	pause := c.pauses[key]
	delete(c.pauses, key)
	c.mu.Unlock()
	if pause == nil {
		return nil
	}
	return pause.wait(ctx)
}

// Pause is one registered pause.
type Pause struct {
	reached     chan struct{}
	release     chan struct{}
	reachOnce   sync.Once
	releaseOnce sync.Once
}

// Reached closes when the simulator reaches the paused boundary.
func (p *Pause) Reached() <-chan struct{} {
	return p.reached
}

// Release unblocks the paused call. It is safe to call more than once and
// before the boundary is reached.
func (p *Pause) Release() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func (p *Pause) wait(ctx context.Context) error {
	p.reachOnce.Do(func() { close(p.reached) })
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EffectKind names what a scripted external effect did.
type EffectKind string

// Effect is one external effect a simulator applied, in application order.
type Effect struct {
	Role      contracts.Role
	Operation ids.OperationID
	Method    contracts.Method
	Kind      EffectKind
	Detail    string
}

// Ledger records what a simulator witnessed and did: delivered requests,
// cancels, opened subscriptions and applied effects. It is the evidence that
// redelivery never duplicated external work: Deliveries counts attempts and
// Effects counts applications, so an applied operation redelivered twice has
// one effect and two deliveries.
type Ledger struct {
	mu            sync.Mutex
	sent          []contracts.Request
	cancels       []contracts.Cancel
	subscriptions []contracts.Cursor
	deliveries    map[ids.OperationID]int
	cancelsByOp   map[ids.OperationID]int
	effects       map[ids.OperationID]int
	applied       []Effect
}

func newLedger() *Ledger {
	return &Ledger{
		deliveries:  map[ids.OperationID]int{},
		cancelsByOp: map[ids.OperationID]int{},
		effects:     map[ids.OperationID]int{},
	}
}

func (l *Ledger) recordRequest(request contracts.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, request)
	l.deliveries[request.Operation]++
}

func (l *Ledger) recordCancel(cancel contracts.Cancel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancels = append(l.cancels, cancel)
	l.cancelsByOp[cancel.Operation]++
}

func (l *Ledger) recordSubscription(cursor contracts.Cursor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subscriptions = append(l.subscriptions, cursor)
}

func (l *Ledger) recordEffect(effect Effect) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applied = append(l.applied, effect)
	l.effects[effect.Operation]++
}

// Deliveries returns how many times the operation was delivered.
func (l *Ledger) Deliveries(operation ids.OperationID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deliveries[operation]
}

// Effects returns how many external effects the operation applied.
func (l *Ledger) Effects(operation ids.OperationID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effects[operation]
}

// Cancels returns how many cancels named the operation.
func (l *Ledger) Cancels(operation ids.OperationID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancelsByOp[operation]
}

// Sent returns the delivered requests in arrival order.
func (l *Ledger) Sent() []contracts.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sent)
}

// Subscriptions returns the cursors subscriptions opened from, in order.
func (l *Ledger) Subscriptions() []contracts.Cursor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.subscriptions)
}

// Cancelled returns the cancels in arrival order.
func (l *Ledger) Cancelled() []contracts.Cancel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.cancels)
}

// Applied returns the applied effects in application order.
func (l *Ledger) Applied() []Effect {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.applied)
}

// Reply is a scripted provider reply without the operation and delivery the
// simulator stamps on it when answering.
type Reply struct {
	Outcome    contracts.Outcome
	Durability lifecycle.SendStatus
	Result     json.RawMessage
	Failure    *contracts.Failure
}

// Accepted is a durable success.
func Accepted(result []byte) Reply {
	return Reply{Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted, Result: result}
}

// Pending is a success the provider has durably accepted but not completed.
func Pending(result []byte) Reply {
	return Reply{Outcome: contracts.OutcomeOK, Durability: lifecycle.SendPending, Result: result}
}

// Rejected is a typed failure with no effect.
func Rejected(failure *contracts.Failure) Reply {
	return Reply{Outcome: contracts.OutcomeFailed, Durability: lifecycle.SendRejected, Failure: failure}
}

// Unknown is a typed failure of unknown durability: the product must
// reconcile rather than treat it as no effect.
func Unknown(failure *contracts.Failure) Reply {
	return Reply{Outcome: contracts.OutcomeFailed, Durability: lifecycle.SendUnknown, Failure: failure}
}

// stamp answers one delivery with the scripted reply and validates it.
func (r Reply) stamp(operation ids.OperationID, delivery ids.DeliveryID) (contracts.Reply, error) {
	reply := contracts.Reply{
		Operation: operation, Delivery: delivery, Outcome: r.Outcome,
		Durability: r.Durability, Result: r.Result, Failure: r.Failure,
	}
	if err := reply.Validate(); err != nil {
		return contracts.Reply{}, fmt.Errorf("%w: %v", ErrInvalidScenario, err)
	}
	return reply, nil
}
