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
		if failure.Code == contracts.FailureMalformedFrame {
			return Ingestion{}, failure
		}
		// The operation is in the peer's own tenant, so the refusal is
		// recorded there against it; nothing the envelope claims is written.
		if err := recordForgedEnvelope(ctx, uow, peer, event, operation, at); err != nil {
			return Ingestion{}, err
		}
		if err := uow.Commit(ctx); err != nil {
			return Ingestion{}, err
		}
		committed = true
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

// recordForgedEnvelope records refusal evidence for an envelope whose claimed
// binding does not match the authenticated peer or the recorded operation.
func recordForgedEnvelope(ctx context.Context, uow *store.UnitOfWork, peer contracts.Peer, event contracts.Event, operation lifecycle.ProviderOperation, at time.Time) error {
	code := lifecycle.RejectWrongRecord
	switch {
	case event.Provider != peer.Provider:
		code = lifecycle.RejectUnauthorizedActor
	case event.Tenant != peer.Tenant:
		code = lifecycle.RejectCrossTenant
	}
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return err
	}
	refusal := &lifecycle.TransitionError{Code: code, Change: lifecycle.Change{
		Tenant: peer.Tenant, Record: lifecycle.RecordOperation, RecordID: operation.ID.Untyped(),
		Event: ingestEvent, From: string(operation.State), Version: operation.Version,
	}}
	return store.RecordRefusal(ctx, uow, refusal.Audit(auditID, lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: peer.Provider}, at))
}

// ingestEvent names a refused envelope in audit evidence.
const ingestEvent = "ingest"

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
	case contracts.EntryAttentionRequested:
		outcome, err = applyDecoded(ctx, a, entry, a.attentionRequested)
	case contracts.EntryDecisionConsumed:
		outcome, err = applyDecoded(ctx, a, entry, a.decisionConsumed)
	case contracts.EntryRuntimeStopped:
		outcome, err = applyDecoded(ctx, a, entry, a.runtimeStopped)
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
			// A replay names the same session; a turn reported under another
			// session contradicts its recorded binding.
			return stored.SessionRef == binding.SessionRef, nil
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
	if payload.Outcome == lifecycle.TurnInterrupted {
		// An interrupted turn reports the runtime's own end of work: the
		// stop observation of an attempt already awaiting its stops.
		attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
		if err != nil {
			return 0, err
		}
		if attempt.State == lifecycle.AttemptCancellationRequested {
			return a.applyStop(ctx, entry, lifecycle.AttemptRuntimeStopped)
		}
		return entryApplied, nil
	}
	if payload.Outcome != lifecycle.TurnFailed {
		return entryApplied, nil
	}
	attempt, applied, refusal, err := a.step.Attempt(ctx, a.event.Attempt, lifecycle.AttemptFail, "")
	if err != nil {
		return 0, err
	}
	if !applied && attempt.State == lifecycle.AttemptReconciling {
		// A failed turn is a definite observation that the work ran and
		// failed: it resolves the reconciliation, so a later outcome for the
		// uncertain operation cannot return the Attempt to executing.
		if _, applied, refusal, err = a.step.Attempt(ctx, a.event.Attempt, lifecycle.AttemptReconciled, lifecycle.AttemptFailed); err != nil {
			return 0, err
		}
	}
	if !applied && refusal != nil {
		// A failure reported for an Attempt that already ended is late: it is
		// retained as evidence and never overrides the current state.
		return entryRefused, a.record(ctx, refusal)
	}
	// A failed run cannot serve its wait: its open native request supersedes,
	// never resurrects.
	if err := a.step.SupersedeOpenAttention(ctx, a.event.Attempt); err != nil {
		return 0, err
	}
	return entryApplied, nil
}

// environmentObserved records an observation of a resource bound to this
// Attempt, binding it from the event's operation when first seen. The
// product stores observed state; it does not control compute. An environment
// observed absent or hibernated while the Attempt awaits its stops is a
// definite stop observation; every other state is progress only.
func (a entryApplier) environmentObserved(ctx context.Context, entry contracts.Entry, payload contracts.EnvironmentObserved) (entryOutcome, error) {
	ok, err := a.bind(ctx, lifecycle.ResourceEnvironmentResource, payload.ResourceRef, "")
	if err != nil {
		return 0, err
	}
	if !ok {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	observation, replayed, err := store.UpsertObservation(ctx, a.uow, lifecycle.EnvironmentObservation{
		Tenant: a.event.Tenant, Attempt: a.event.Attempt, Provider: a.event.Provider,
		ResourceRef: payload.ResourceRef, State: payload.State, Sequence: payload.Sequence,
		ObservedAt: payload.ObservedAt.UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		return 0, err
	}
	if replayed {
		// A stale or repeated observation advances ingestion only. It cannot
		// prove a stop contradicted by the newer state already stored.
		return entryProgress, nil
	}
	if observation.Stopped() {
		attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
		if err != nil {
			return 0, err
		}
		if attempt.State == lifecycle.AttemptCancellationRequested {
			return a.applyStop(ctx, entry, lifecycle.AttemptEnvironmentStopped)
		}
	}
	return entryApplied, nil
}

// attentionRequested raises a product attention request for the event's
// Attempt from a provider observation: the request never names product
// identities itself, and the one native reference maps to exactly one
// request. The attempt then awaits and the task waits; the frozen machines
// refuse an attempt that cannot await, and every refusal is evidence.
func (a entryApplier) attentionRequested(ctx context.Context, entry contracts.Entry, payload contracts.AttentionRequested) (entryOutcome, error) {
	attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
	if err != nil {
		return 0, err
	}
	task, err := store.GetTask(ctx, a.uow, a.event.Tenant, attempt.Task)
	if err != nil {
		return 0, err
	}
	// One blocking request per attempt: an unconsumed wait means this one
	// cannot join.
	open, err := store.ListOpenAttentionByAttempt(ctx, a.uow, a.event.Tenant, attempt.ID)
	if err != nil {
		return 0, err
	}
	if len(open) > 0 {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	// A native reference the provider already raised resolves to its first
	// request; it is provenance, never a rebind.
	if _, err := store.GetAttentionNative(ctx, a.uow, a.event.Tenant, a.event.Provider, payload.NativeRef); err == nil {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	policy, err := store.GetPolicy(ctx, a.uow, a.event.Tenant)
	if err != nil {
		return 0, err
	}
	attentionID, err := ids.New[ids.Attention]()
	if err != nil {
		return 0, err
	}
	request := lifecycle.AttentionRequest{
		ID: attentionID, Tenant: a.event.Tenant, Task: attempt.Task, Attempt: attempt.ID,
		Kind: payload.Kind, PolicyGeneration: policy.Generation,
		Responders: []ids.PrincipalID{task.Owner},
		ExpiresAt:  payload.ExpiresAt.UTC().Truncate(time.Microsecond),
	}
	if payload.Kind == lifecycle.AttentionActionApproval {
		request.Provider, request.Operation = a.event.Provider, a.event.Operation
		request.ActionDigest = payload.ActionDigest
	}
	if !request.ExpiresAt.After(a.step.At) {
		return a.refuse(ctx, entry, lifecycle.RejectRequestExpired)
	}
	request, err = lifecycle.NewAttentionRequest(request, a.step.At)
	if err != nil {
		return a.refuse(ctx, entry, lifecycle.RejectInvalidTransition)
	}
	// The wait is taken first: an Attempt that cannot await refuses the
	// whole observation, so nothing is written for a request that could
	// never block its work.
	if _, applied, refusal, err := a.step.Attempt(ctx, a.event.Attempt, lifecycle.AttemptAwaitInput, ""); err != nil {
		return 0, err
	} else if !applied && refusal != nil {
		return entryRefused, a.record(ctx, refusal)
	} else if applied {
		if _, err := a.step.Task(ctx, attempt.Task, lifecycle.TaskAwaitInput); err != nil {
			return 0, err
		}
	}
	if _, _, err := store.InsertAttention(ctx, a.uow, request); err != nil {
		if errors.Is(err, store.ErrPayloadMismatch) {
			return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
		}
		return 0, err
	}
	// The correlation row binds the native reference under the provider that
	// reported it; for an approval it also records the governed action class
	// the observation named, the provenance a decision needs.
	native := store.AttentionNative{
		Tenant: a.event.Tenant, Provider: a.event.Provider, NativeRef: payload.NativeRef,
		Attention: request.ID, ActionClass: payload.Action, CreatedAt: a.step.At,
	}
	if _, _, err := store.InsertAttentionNative(ctx, a.uow, native); err != nil {
		if errors.Is(err, store.ErrPayloadMismatch) {
			return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
		}
		return 0, err
	}
	return entryApplied, nil
}

// decisionConsumed records the runtime consuming a delivered decision,
// resolved through the native reference under this event's provider and
// attempt. The request closes, the attempt resumes and the task leaves its
// wait; a request not yet delivered, or one closed without delivery, is
// refused as evidence — a consumed observation is never a route to a state
// the machine did not reach.
func (a entryApplier) decisionConsumed(ctx context.Context, entry contracts.Entry, payload contracts.DecisionConsumed) (entryOutcome, error) {
	native, err := store.GetAttentionNative(ctx, a.uow, a.event.Tenant, a.event.Provider, payload.NativeRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
		}
		return 0, err
	}
	attention, err := store.GetAttention(ctx, a.uow, a.event.Tenant, native.Attention)
	if err != nil {
		return 0, err
	}
	if attention.Attempt != a.event.Attempt {
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return 0, err
	}
	consumed, replayed, err := store.ApplyAttention(ctx, a.uow, a.event.Tenant, attention.ID, lifecycle.AttentionConsume, store.Transition{
		ExpectedVersion: attention.Version, At: a.step.At, Actor: a.step.Actor, AuditID: auditID,
	})
	if err != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(err, &refusal) {
			return entryRefused, a.record(ctx, refusal)
		}
		return 0, err
	}
	if replayed {
		return entryApplied, nil
	}
	_, applied, refusal, err := a.step.Attempt(ctx, a.event.Attempt, lifecycle.AttemptResume, "")
	if err != nil {
		return 0, err
	}
	if !applied && refusal != nil {
		// The wait resolved but the attempt can no longer resume it: the
		// contradiction is evidence, and the consumption stays recorded.
		return entryRefused, a.record(ctx, refusal)
	}
	if _, err := a.step.Task(ctx, consumed.Task, lifecycle.TaskInputResolved); err != nil {
		return 0, err
	}
	return entryApplied, nil
}

// runtimeStopped applies the runtime's stop observation. While the attempt
// awaits its stops it converges the cancellation — the machines order the
// runtime and environment observations and refuse a repeated one. An attempt
// that already ended while its task still stands cancelling confirms the
// task's stop: cancellation is intent until the work ended, whichever way it
// ended.
func (a entryApplier) runtimeStopped(ctx context.Context, entry contracts.Entry, _ contracts.RuntimeStopped) (entryOutcome, error) {
	attempt, err := store.GetAttempt(ctx, a.uow, a.event.Tenant, a.event.Attempt)
	if err != nil {
		return 0, err
	}
	if attempt.State == lifecycle.AttemptTerminated {
		task, err := store.GetTask(ctx, a.uow, a.event.Tenant, attempt.Task)
		if err != nil {
			return 0, err
		}
		if task.State != lifecycle.TaskCancelling {
			return entryApplied, nil
		}
		if _, err := a.step.Task(ctx, task.ID, lifecycle.TaskStopConfirmed); err != nil {
			return 0, err
		}
		return entryApplied, nil
	}
	if attempt.State != lifecycle.AttemptCancellationRequested {
		// A runtime stop without a requested stop: no work may end
		// unprompted, so the observation is refused, not applied.
		return a.refuse(ctx, entry, lifecycle.RejectWrongRecord)
	}
	return a.applyStop(ctx, entry, lifecycle.AttemptRuntimeStopped)
}

// applyStop applies one stop observation to an attempt already awaiting its
// stops. A refusal — a repeated observation of the same stop, or one
// contradicting the current state — is recorded evidence, never an override;
// the attempt's terminal move releases the reservation through the progress
// step.
func (a entryApplier) applyStop(ctx context.Context, entry contracts.Entry, event lifecycle.AttemptEvent) (entryOutcome, error) {
	_, applied, refusal, err := a.step.Attempt(ctx, a.event.Attempt, event, "")
	if err != nil {
		return 0, err
	}
	if !applied && refusal != nil {
		return entryRefused, a.record(ctx, refusal)
	}
	return entryApplied, nil
}

// operationOutcome reconciles an uncertain operation to the provider's
// reported state, the worker's reconciliation semantics, and the Attempt
// follows. An outcome for an operation that is not uncertain changes nothing:
// it is never a second route to completion.
func (a entryApplier) operationOutcome(ctx context.Context, _ contracts.Entry, payload contracts.OperationOutcome) (entryOutcome, error) {
	current, err := store.GetOperation(ctx, a.uow, a.event.Tenant, a.event.Operation)
	if err != nil {
		return 0, err
	}
	if current.State != lifecycle.OperationUncertain {
		return entryProgress, nil
	}
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return 0, err
	}
	if current, _, err = store.ApplyOperation(ctx, a.uow, a.event.Tenant, current.ID, lifecycle.OperationReconcile, payload.State, store.Transition{
		ExpectedVersion: current.Version, At: a.step.At, Actor: a.step.Actor, AuditID: auditID,
	}); err != nil {
		return 0, err
	}
	if err := a.step.FollowOperation(ctx, current); err != nil {
		return 0, err
	}
	return entryApplied, nil
}
