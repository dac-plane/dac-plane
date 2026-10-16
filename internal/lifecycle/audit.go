package lifecycle

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// ErrInvalid reports a malformed lifecycle record or input.
var ErrInvalid = errors.New("invalid lifecycle contract value")

// RecordKind names the authoritative record a change applies to.
type RecordKind string

const (
	RecordTask      RecordKind = "task"
	RecordAttempt   RecordKind = "attempt"
	RecordAttention RecordKind = "attention_request"
	RecordOperation RecordKind = "provider_operation"
	RecordHarness   RecordKind = "harness_qualification"
	RecordMigration RecordKind = "migration_pass"
	RecordClaim     RecordKind = "maintenance_claim"
)

// RecordKinds lists every valid RecordKind.
func RecordKinds() []RecordKind {
	return []RecordKind{
		RecordTask, RecordAttempt, RecordAttention, RecordOperation, RecordHarness, RecordMigration, RecordClaim,
	}
}

// RejectionCode is why a transition was refused.
type RejectionCode string

const (
	RejectVersionConflict     RejectionCode = "version_conflict"
	RejectTerminalState       RejectionCode = "terminal_state"
	RejectInvalidTransition   RejectionCode = "invalid_transition"
	RejectAmbiguousTarget     RejectionCode = "ambiguous_target"
	RejectInvalidTarget       RejectionCode = "invalid_target"
	RejectCrossTenant         RejectionCode = "cross_tenant"
	RejectWrongRecord         RejectionCode = "wrong_record"
	RejectUnauthorizedActor   RejectionCode = "unauthorized_actor"
	RejectDecisionNotAllowed  RejectionCode = "decision_not_allowed"
	RejectRequestExpired      RejectionCode = "request_expired"
	RejectStopAlreadyObserved RejectionCode = "stop_already_observed"
	RejectStaleClaim          RejectionCode = "stale_claim"
	RejectClaimHeld           RejectionCode = "claim_held"
	RejectStalePolicy         RejectionCode = "stale_policy"
)

// RejectionCodes lists every valid RejectionCode.
func RejectionCodes() []RejectionCode {
	return []RejectionCode{
		RejectVersionConflict, RejectTerminalState, RejectInvalidTransition, RejectAmbiguousTarget,
		RejectInvalidTarget, RejectCrossTenant, RejectWrongRecord, RejectUnauthorizedActor,
		RejectDecisionNotAllowed, RejectRequestExpired, RejectStopAlreadyObserved, RejectStaleClaim,
		RejectClaimHeld, RejectStalePolicy,
	}
}

// Change describes one transition decision on one record: the event, the state
// it was decided from, and for an applied change the resulting state and version.
type Change struct {
	Tenant   ids.TenantID
	Record   RecordKind
	RecordID ids.AnyID
	Event    string
	From     string
	To       string
	Version  uint64
}

// Audit records the applied change as performed by actor.
func (c Change) Audit(id ids.AuditID, actor Actor, at time.Time) AuditEvent {
	return AuditEvent{
		ID: id, Tenant: c.Tenant, Record: c.Record, RecordID: c.RecordID, Actor: actor,
		Event: c.Event, From: c.From, To: c.To, Version: c.Version,
		Outcome: AuditApplied, OccurredAt: at,
	}
}

// TransitionError is a refused transition. It carries the record context so the
// refusal itself can be audited: a late or illegal provider observation is
// recorded this way and never applied.
type TransitionError struct {
	Change Change
	Code   RejectionCode
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("%s %s: %s from %s rejected: %s", e.Change.Record, e.Change.RecordID, e.Change.Event, e.Change.From, e.Code)
}

// Audit records the refusal as attempted by actor.
func (e *TransitionError) Audit(id ids.AuditID, actor Actor, at time.Time) AuditEvent {
	event := e.Change.Audit(id, actor, at)
	event.To = ""
	event.Outcome = AuditRejected
	event.Reason = e.Code
	return event
}

// RejectionOf returns the code of a TransitionError, or "" for other errors.
func RejectionOf(err error) RejectionCode {
	var transitionErr *TransitionError
	if errors.As(err, &transitionErr) {
		return transitionErr.Code
	}
	return ""
}

func reject(change Change, code RejectionCode) error {
	change.To = ""
	return &TransitionError{Change: change, Code: code}
}

// ActorKind is who caused a change.
type ActorKind string

const (
	ActorPrincipal ActorKind = "principal"
	ActorProvider  ActorKind = "provider"
	ActorSystem    ActorKind = "system"
)

// ActorKinds lists every valid ActorKind.
func ActorKinds() []ActorKind {
	return []ActorKind{ActorPrincipal, ActorProvider, ActorSystem}
}

// Actor attributes a change: a principal's command, a provider observation or
// the control plane itself (reconciliation, expiry).
type Actor struct {
	Kind      ActorKind       `json:"kind"`
	Principal ids.PrincipalID `json:"principal_id,omitzero"`
	Provider  ids.ProviderID  `json:"provider_id,omitzero"`
}

// Validate reports whether exactly the identifier for the kind is set.
func (a Actor) Validate() error {
	valid := false
	switch a.Kind {
	case ActorPrincipal:
		valid = !a.Principal.IsZero() && a.Provider.IsZero()
	case ActorProvider:
		valid = a.Principal.IsZero() && !a.Provider.IsZero()
	case ActorSystem:
		valid = a.Principal.IsZero() && a.Provider.IsZero()
	}
	if !valid {
		return fmt.Errorf("%w: actor identifiers do not match its kind", ErrInvalid)
	}
	return nil
}

// AuditOutcome is whether the audited change was applied or refused.
type AuditOutcome string

const (
	AuditApplied  AuditOutcome = "applied"
	AuditRejected AuditOutcome = "rejected"
)

// AuditOutcomes lists every valid AuditOutcome.
func AuditOutcomes() []AuditOutcome {
	return []AuditOutcome{AuditApplied, AuditRejected}
}

// AuditEvent is the append-only record of one transition decision: who asked
// for what change to which record, and what happened. It is not a domain event
// for consumers and not an outbox delivery record; those are separate (#20).
// It carries state names and identifiers only, never payloads.
type AuditEvent struct {
	ID         ids.AuditID   `json:"audit_id"`
	Tenant     ids.TenantID  `json:"tenant_id"`
	Record     RecordKind    `json:"record"`
	RecordID   ids.AnyID     `json:"record_id"`
	Actor      Actor         `json:"actor"`
	Event      string        `json:"event"`
	From       string        `json:"from_state"`
	To         string        `json:"to_state,omitzero"`
	Version    uint64        `json:"version"`
	Outcome    AuditOutcome  `json:"outcome"`
	Reason     RejectionCode `json:"reason,omitzero"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// Validate reports whether the event is complete and consistent with its outcome.
func (e AuditEvent) Validate() error {
	if e.ID.IsZero() || e.Tenant.IsZero() || e.RecordID.IsZero() || e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: audit event requires identifiers and time", ErrInvalid)
	}
	if !slices.Contains(RecordKinds(), e.Record) || e.Event == "" || e.From == "" || e.Version == 0 {
		return fmt.Errorf("%w: audit event requires record kind, event, from state and version", ErrInvalid)
	}
	if err := e.Actor.Validate(); err != nil {
		return err
	}
	switch e.Outcome {
	case AuditApplied:
		if e.To == "" || e.Reason != "" {
			return fmt.Errorf("%w: applied audit event requires a target state and no reason", ErrInvalid)
		}
	case AuditRejected:
		if e.To != "" || !slices.Contains(RejectionCodes(), e.Reason) {
			return fmt.Errorf("%w: rejected audit event requires a known reason and no target state", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown audit outcome", ErrInvalid)
	}
	return nil
}
