// Package application is the transport-neutral command boundary of the
// control plane. Every mutating command is authenticated (it carries an
// identity.AuthorizationContext), authorized (the principal must exist in its
// tenant), idempotent (a client nonce binds the command to the fingerprint of
// its first payload and to its outcome) and version checked where it changes
// an existing record. Each command runs in one transaction: its records,
// audit, durable events and outbox entries commit together or not at all.
//
// The package composes the domain records (internal/lifecycle) and the store
// (internal/platform/postgres); it holds no transition rules of its own and
// no transport types.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

const maxNonceLength = 128

var (
	// ErrInvalidCommand reports a command that fails boundary validation.
	// Nothing is written.
	ErrInvalidCommand = errors.New("invalid command")
	// ErrUnauthorized reports an actor that is not a principal of its tenant.
	ErrUnauthorized = errors.New("principal is not authorized in this tenant")
	// ErrNoHarness reports a provider with no qualified harness: absent, or
	// qualified off.
	ErrNoHarness = errors.New("no qualified harness for the selected provider")
	// ErrNoEnvironment reports that the selected environment provider is not
	// registered for this tenant.
	ErrNoEnvironment = errors.New("no registered environment provider for the selected id")
	// ErrPayloadMismatch reports a nonce reused with a different payload. The
	// original outcome is untouched.
	ErrPayloadMismatch = store.ErrPayloadMismatch
	// ErrNotFound reports a submission that does not exist for this actor.
	ErrNotFound = store.ErrNotFound
)

// Command is the envelope every command carries. Nonce is the client's
// idempotency key, scoped to the actor's tenant, principal and command kind.
// ExpectedVersion is the version of the record a command changes; commands
// that create records leave it zero.
type Command struct {
	Actor           identity.AuthorizationContext
	Nonce           string
	ExpectedVersion uint64
}

// validate checks the envelope. creating tells whether the command creates
// its records, in which case no expected version applies.
func (c Command) validate(creating bool) error {
	if err := c.Actor.Principal().Validate(); err != nil {
		return fmt.Errorf("%w: an authenticated principal is required", ErrInvalidCommand)
	}
	if c.Nonce == "" || len(c.Nonce) > maxNonceLength {
		return fmt.Errorf("%w: the nonce must be 1 to %d bytes", ErrInvalidCommand, maxNonceLength)
	}
	if creating && c.ExpectedVersion != 0 {
		return fmt.Errorf("%w: a creating command takes no expected version", ErrInvalidCommand)
	}
	if !creating && c.ExpectedVersion == 0 {
		return fmt.Errorf("%w: an expected version is required", ErrInvalidCommand)
	}
	return nil
}

// actor is the lifecycle actor of the command's principal.
func (c Command) actor() lifecycle.Actor {
	return lifecycle.Actor{Kind: lifecycle.ActorPrincipal, Principal: c.Actor.Principal().ID}
}

// fingerprint is the digest of a command's payload: its canonical JSON form,
// excluding the nonce and the command time, so a retry made later with the
// same request has the same fingerprint.
func fingerprint(kind string, payload any) (identity.Digest, error) {
	data, err := json.Marshal(struct {
		Command string `json:"command"`
		Payload any    `json:"payload"`
	}{kind, payload})
	if err != nil {
		return "", err
	}
	return identity.DigestOf(data), nil
}

// unit is one command's transaction plus the transition stamps it issued, so
// a refused step can be re-recorded after the rollback.
type unit struct {
	uow   *store.UnitOfWork
	actor lifecycle.Actor
	at    time.Time
	last  store.Transition
	// lastAt is when the last transition's refusal would be audited; the
	// store audits spend assertions at their assertion time.
	lastAt time.Time
}

// transition returns a fresh transition stamp for one store step.
func (u *unit) transition(expectedVersion uint64, outbox *store.OutboxPayload) (store.Transition, error) {
	auditID, err := ids.New[ids.Audit]()
	if err != nil {
		return store.Transition{}, err
	}
	u.last = store.Transition{ExpectedVersion: expectedVersion, At: u.at, Actor: u.actor, AuditID: auditID, Outbox: outbox}
	u.lastAt = u.at
	return u.last, nil
}

// execute runs one idempotent command: it reserves the nonce, runs work and
// records work's result, all in one transaction. A reserved nonce with the
// same fingerprint replays the stored result; a different fingerprint is
// ErrPayloadMismatch. A domain refusal rolls everything back, then the
// refusal evidence is recorded on its own, so nothing partial commits.
func execute(ctx context.Context, pool *pgxpool.Pool, cmd Command, kind string, digest identity.Digest, at time.Time, work func(*unit) (json.RawMessage, error)) (json.RawMessage, error) {
	return executeAuthorized(ctx, pool, cmd, kind, digest, at, nil, work)
}

// executeAuthorized supports boundaries whose current permission must also
// authorize replay reads. The permission lock lasts through the transaction.
func executeAuthorized(ctx context.Context, pool *pgxpool.Pool, cmd Command, kind string, digest identity.Digest, at time.Time, authorize func(*store.UnitOfWork) error, work func(*unit) (json.RawMessage, error)) (json.RawMessage, error) {
	principal := cmd.Actor.Principal()
	uow, err := store.Begin(ctx, pool)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()

	switch stored, err := store.GetPrincipal(ctx, uow, principal.Tenant, principal.ID); {
	case errors.Is(err, store.ErrNotFound), err == nil && stored != principal:
		return nil, ErrUnauthorized
	case err != nil:
		return nil, err
	}
	if authorize != nil {
		if err := authorize(uow); err != nil {
			return nil, err
		}
	}
	switch inserted, err := store.InsertCommandRecord(ctx, uow, store.CommandRecord{
		Tenant: principal.Tenant, Principal: principal.ID, Command: kind, Nonce: cmd.Nonce,
		RequestDigest: digest, CreatedAt: at,
	}); {
	case err != nil:
		return nil, err
	case !inserted:
		// The nonce is already bound: committed earlier, or by a concurrent
		// duplicate this insert waited for. Replay it.
		record, err := store.GetCommandRecord(ctx, uow, principal.Tenant, principal.ID, kind, cmd.Nonce)
		if err != nil {
			return nil, err
		}
		if record.RequestDigest != digest {
			return nil, ErrPayloadMismatch
		}
		if err := uow.Commit(ctx); err != nil {
			return nil, err
		}
		committed = true
		return record.Result, nil
	}

	u := &unit{uow: uow, actor: cmd.actor(), at: at}
	result, err := work(u)
	if err != nil {
		var refusal *lifecycle.TransitionError
		if errors.As(err, &refusal) && !u.last.AuditID.IsZero() {
			_ = uow.Rollback(ctx)
			committed = true
			if recordErr := store.RecordRefusal(ctx, pool, refusal.Audit(u.last.AuditID, u.actor, u.lastAt)); recordErr != nil {
				return nil, errors.Join(err, recordErr)
			}
		}
		return nil, err
	}
	if err := store.SetCommandResult(ctx, uow, principal.Tenant, principal.ID, kind, cmd.Nonce, result); err != nil {
		return nil, err
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}
