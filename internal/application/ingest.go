package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

// Ingestion is what one provider event contributed.
type Ingestion struct {
	// Cursor is the stream's ingestion progress after the event. It records
	// ingestion only; it proves no provider effect.
	Cursor contracts.Cursor
	// Applied counts entries with product effects, Progress entries that were
	// acknowledged and change nothing, Refused entries refused with evidence.
	Applied, Progress, Refused int
	// Duplicates counts entries and tombstones at or below the cursor;
	// Tombstones counts newly acknowledged deletions.
	Duplicates, Tombstones   int
	Replay, Stale, Rehydrate bool
}

// Ingest applies one provider event. peer is the identity the transport
// authenticated; the event's claimed binding is checked against it and
// against the recorded provider operation before anything is written, and a
// forged or mismatched envelope is refused with a typed failure and no
// mutation. Accepted entries are applied in order, together with the cursor,
// in one transaction. Duplicates at or below the cursor are skipped, and
// every effect also has a natural key, so a replay never repeats a
// transition or a record. Cleared and cursor-too-old report Rehydrate;
// missing history is never read as success.
func Ingest(ctx context.Context, pool *pgxpool.Pool, peer contracts.Peer, event contracts.Event, at time.Time) (Ingestion, error) {
	at = at.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return Ingestion{}, fmt.Errorf("%w: an ingestion time is required", ErrInvalidCommand)
	}
	if peer.Tenant.IsZero() || peer.Provider.IsZero() {
		return Ingestion{}, &contracts.Failure{Code: contracts.FailureUnauthenticated, Message: "no authenticated provider"}
	}
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		return Ingestion{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()

	// The operation is looked up in the authenticated tenant only: another
	// tenant's operation reads as absent.
	operation, err := store.GetOperation(ctx, uow, peer.Tenant, event.Operation)
	if errors.Is(err, store.ErrNotFound) {
		return Ingestion{}, &contracts.Failure{Code: contracts.FailureUnauthorized, Message: "event is not bound to a recorded operation"}
	}
	if err != nil {
		return Ingestion{}, err
	}
	bound, failure := contracts.BindEvent(peer, event, operation)
	if failure != nil {
		return Ingestion{}, failure
	}
	cursor, err := store.LockCursor(ctx, uow, contracts.Cursor{
		Tenant: event.Tenant, Attempt: event.Attempt, Operation: event.Operation,
		Stream: event.Stream, Generation: event.Generation,
	})
	if err != nil {
		return Ingestion{}, err
	}
	ingested, failure := contracts.Ingest(cursor, bound)
	if failure != nil {
		return Ingestion{}, failure
	}
	result := Ingestion{
		Cursor: ingested.Cursor, Duplicates: ingested.Duplicates, Tombstones: len(ingested.Tombstones),
		Replay: ingested.Replay, Stale: ingested.Stale, Rehydrate: ingested.Rehydrate,
	}
	applier := entryApplier{
		uow: uow, event: event, operation: operation,
		step: progress.Step{
			UOW: uow, Tenant: event.Tenant, At: at,
			Actor: lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: peer.Provider},
		},
	}
	for _, entry := range ingested.Entries {
		outcome, err := applier.apply(ctx, entry)
		if err != nil {
			return Ingestion{}, err
		}
		switch outcome {
		case entryApplied:
			result.Applied++
		case entryRefused:
			result.Refused++
		default:
			result.Progress++
		}
	}
	if _, err := store.UpsertCursor(ctx, uow, result.Cursor); err != nil {
		return Ingestion{}, err
	}
	if err := uow.Commit(ctx); err != nil {
		return Ingestion{}, err
	}
	committed = true
	return result, nil
}

type entryOutcome int

const (
	entryProgress entryOutcome = iota
	entryApplied
	entryRefused
)

// entryApplier turns accepted entries into records and transitions. Every
// identity comes from the bound event; payloads only describe observations.
type entryApplier struct {
	uow       *store.UnitOfWork
	event     contracts.Event
	operation lifecycle.ProviderOperation
	step      progress.Step
}

func (a entryApplier) apply(ctx context.Context, entry contracts.Entry) (entryOutcome, error) {
	switch contracts.ClassifyEntry(entry.Kind) {
	case contracts.EntryProgress:
		return entryProgress, nil
	case contracts.EntryRefuse:
		// An observation cannot grant permission or change state by naming
		// itself; it is recorded and never applied.
		return a.refuse(ctx, entry, lifecycle.RejectUnauthorizedActor)
	}
	var err error
	var outcome entryOutcome
	switch entry.Kind {
	case contracts.EntryRuntimeStarted:
		outcome, err = applyDecoded(ctx, a, entry, a.runtimeStarted)
	case contracts.EntryTurnStarted:
		outcome, err = applyDecoded(ctx, a, entry, a.turnStarted)
	case contracts.EntryTurnFinished:
		outcome, err = applyDecoded(ctx, a, entry, a.turnFinished)
	case contracts.EntryEnvironmentObserved:
		outcome, err = applyDecoded(ctx, a, entry, a.environmentObserved)
	case contracts.EntryOperationOutcome:
		outcome, err = applyDecoded(ctx, a, entry, a.operationOutcome)
	}
	return outcome, err
}

// applyDecoded decodes an entry's payload and applies it; a payload that
// does not decode is refused with evidence.
func applyDecoded[T interface{ Validate() error }](ctx context.Context, a entryApplier, entry contracts.Entry, apply func(context.Context, contracts.Entry, T) (entryOutcome, error)) (entryOutcome, error) {
	payload, err := contracts.DecodeEntry[T](entry)
	if err != nil {
		return a.refuse(ctx, entry, lifecycle.RejectInvalidTransition)
	}
	return apply(ctx, entry, payload)
}

// refuse records refusal evidence against the event's Attempt. The entry is
// acknowledged (the cursor moves past it) and never applied.
func (a entryApplier) refuse(ctx context.Context, entry contracts.Entry, code lifecycle.RejectionCode) (entryOutcome, error) {
	attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
	if err != nil {
		return 0, err
	}
	return entryRefused, a.record(ctx, &lifecycle.TransitionError{Code: code, Change: lifecycle.Change{
		Tenant: a.event.Tenant, Record: lifecycle.RecordAttempt, RecordID: attempt.ID.Untyped(),
		Event: entry.Kind, From: string(attempt.State), Version: attempt.Version,
	}})
}

func (a entryApplier) record(ctx context.Context, refusal *lifecycle.TransitionError) error {
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return err
	}
	return store.RecordRefusal(ctx, a.uow, refusal.Audit(auditID, a.step.Actor, a.step.At))
}

// bind records a native resource for the event's Attempt and operation. It
// refuses a resource already bound to another Attempt; rebinding the same
// resource to the same Attempt is a replay.
func (a entryApplier) bind(ctx context.Context, kind lifecycle.ResourceKind, nativeRef, sessionRef string) (bool, error) {
	binding := lifecycle.Binding{
		Tenant: a.event.Tenant, Attempt: a.event.Attempt, Provider: a.event.Provider, Operation: a.event.Operation,
		Kind: kind, NativeRef: nativeRef, SessionRef: sessionRef, BoundAt: a.step.At,
	}
	existing, err := store.ListBindings(ctx, a.uow, binding.Tenant, binding.Provider, kind, nativeRef)
	if err != nil {
		return false, err
	}
	for _, stored := range existing {
		if stored.Attempt == binding.Attempt {
			return true, nil
		}
	}
	if err := lifecycle.CheckBinding(existing, binding); err != nil {
		if errors.Is(err, lifecycle.ErrBindingConflict) {
			return false, nil
		}
		return false, err
	}
	_, _, err = store.InsertBinding(ctx, a.uow, binding)
	return err == nil, err
}

// boundSession reports whether the session is bound to the event's Attempt.
func (a entryApplier) boundSession(ctx context.Context, sessionRef string) (bool, error) {
	existing, err := store.ListBindings(ctx, a.uow, a.event.Tenant, a.event.Provider, lifecycle.ResourceRuntimeSession, sessionRef)
	if err != nil {
		return false, err
	}
	for _, stored := range existing {
		if stored.Attempt == a.event.Attempt {
			return true, nil
		}
	}
	return false, nil
}

// runtimeStarted binds the session and moves a dispatched Attempt to
// executing. An Attempt that already progressed is left as it is.
func (a entryApplier) runtimeStarted(ctx context.Context, entry contracts.Entry, payload contracts.RuntimeStarted) (entryOutcome, error) {
	ok, err := a.bind(ctx, lifecycle.ResourceRuntimeSession, payload.NativeRef, "")
	if err != nil || !ok {
		if err != nil {
			return 0, err
		}
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	for _, event := range []lifecycle.AttemptEvent{lifecycle.AttemptStart, lifecycle.AttemptRuntimeStarted} {
		if _, _, _, err := a.step.Attempt(ctx, a.event.Attempt, event, ""); err != nil {
			return 0, err
		}
	}
	return entryApplied, nil
}

// turnStarted binds a turn of a session already bound to this Attempt.
func (a entryApplier) turnStarted(ctx context.Context, entry contracts.Entry, payload contracts.TurnStarted) (entryOutcome, error) {
	if ok, err := a.boundSession(ctx, payload.SessionRef); err != nil || !ok {
		if err != nil {
			return 0, err
		}
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	ok, err := a.bind(ctx, lifecycle.ResourceRuntimeTurn, payload.NativeRef, payload.SessionRef)
	if err != nil {
		return 0, err
	}
	if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	return entryApplied, nil
}

// turnFinished records the turn end. The turn is bound from the event's
// operation when its start was not seen, so a failure keeps its operation.
// Only a failure changes state: a completed turn is not a proposed result.
func (a entryApplier) turnFinished(ctx context.Context, entry contracts.Entry, payload contracts.TurnFinished) (entryOutcome, error) {
	if ok, err := a.boundSession(ctx, payload.SessionRef); err != nil || !ok {
		if err != nil {
			return 0, err
		}
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	ok, err := a.bind(ctx, lifecycle.ResourceRuntimeTurn, payload.TurnRef, payload.SessionRef)
	if err != nil {
		return 0, err
	}
	if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	end := lifecycle.TurnEnd{
		Tenant: a.event.Tenant, Attempt: a.event.Attempt, Provider: a.event.Provider,
		SessionRef: payload.SessionRef, TurnRef: payload.TurnRef, Outcome: payload.Outcome,
		Failure: payload.Failure, EndedAt: payload.EndedAt.UTC().Truncate(time.Microsecond),
	}
	if _, replayed, err := store.InsertTurnEnd(ctx, a.uow, end); err != nil {
		if errors.Is(err, store.ErrPayloadMismatch) {
			return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
		}
		return 0, err
	} else if replayed {
		return entryApplied, nil
	}
	if payload.Outcome != lifecycle.TurnFailed {
		return entryApplied, nil
	}
	_, applied, refusal, err := a.step.Attempt(ctx, a.event.Attempt, lifecycle.AttemptFail, "")
	if err != nil {
		return 0, err
	}
	if !applied && refusal != nil {
		// A failure reported for an Attempt that already ended is late: it is
		// retained as evidence and never overrides the current state.
		return entryRefused, a.record(ctx, refusal)
	}
	return entryApplied, nil
}

// environmentObserved records an observation of a resource bound to this
// Attempt, binding it from the event's operation when first seen. The
// product stores observed state; it does not control compute.
func (a entryApplier) environmentObserved(ctx context.Context, entry contracts.Entry, payload contracts.EnvironmentObserved) (entryOutcome, error) {
	ok, err := a.bind(ctx, lifecycle.ResourceEnvironmentResource, payload.ResourceRef, "")
	if err != nil {
		return 0, err
	}
	if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	if _, _, err := store.UpsertObservation(ctx, a.uow, lifecycle.EnvironmentObservation{
		Tenant: a.event.Tenant, Attempt: a.event.Attempt, Provider: a.event.Provider,
		ResourceRef: payload.ResourceRef, State: payload.State, Sequence: payload.Sequence,
		ObservedAt: payload.ObservedAt.UTC().Truncate(time.Microsecond),
	}); err != nil {
		return 0, err
	}
	return entryApplied, nil
}

// operationOutcome settles the event's operation from the provider's report:
// an uncertain operation is reconciled, an accepted one completes; either way
// the Attempt follows. Outcomes that no longer apply change nothing.
func (a entryApplier) operationOutcome(ctx context.Context, _ contracts.Entry, payload contracts.OperationOutcome) (entryOutcome, error) {
	current, err := store.GetOperation(ctx, a.uow, a.event.Tenant, a.event.Operation)
	if err != nil {
		return 0, err
	}
	event, target := lifecycle.OperationEvent(""), payload.State
	switch {
	case current.State == lifecycle.OperationUncertain:
		event = lifecycle.OperationReconcile
	case current.State == lifecycle.OperationAccepted && payload.State == lifecycle.OperationSucceeded:
		event, target = lifecycle.OperationSucceed, ""
	case current.State == lifecycle.OperationAccepted && payload.State == lifecycle.OperationFailed:
		event, target = lifecycle.OperationFail, ""
	}
	if event != "" {
		if _, _, check := current.Apply(event, current.Version, target, a.step.At); check == nil {
			auditID, err := ids.New[ids.Audit]()
			if err != nil {
				return 0, err
			}
			if current, _, err = store.ApplyOperation(ctx, a.uow, a.event.Tenant, current.ID, event, target, store.Transition{
				ExpectedVersion: current.Version, At: a.step.At, Actor: a.step.Actor, AuditID: auditID,
			}); err != nil {
				return 0, err
			}
		}
	}
	if err := a.step.FollowOperation(ctx, current); err != nil {
		return 0, err
	}
	return entryApplied, nil
}
