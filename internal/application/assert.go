package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

const commandAssertSpend = "assert_spend"

// AssertSpend is the second phase of a spend-governed submission: the
// principal's assertion of the challenge the submission opened. The receipt
// and the dispatch commit together, so a governed operation is never
// enqueued without both its reservation and a receipt. ExpectedVersion is
// the challenge's version.
type AssertSpend struct {
	Command
	// SubmissionNonce names the governed submission being completed.
	SubmissionNonce string
	// ReviewNonce selects a request_changes scheduling record instead of an
	// initial submission. Exactly one of the two nonces must be supplied.
	ReviewNonce string
	Assertion   lifecycle.SpendAssertion
}

type assertPayload struct {
	Submission      string                   `json:"submission"`
	Review          string                   `json:"review,omitempty"`
	ExpectedVersion uint64                   `json:"expected_version"`
	Assertion       lifecycle.SpendAssertion `json:"assertion"`
}

type assertion struct {
	Submission string `json:"submission"`
	Spend      string `json:"spend"`
}

func (c AssertSpend) validate() error {
	if err := c.Command.validate(false); err != nil {
		return err
	}
	if (c.SubmissionNonce == "") == (c.ReviewNonce == "") || len(c.SubmissionNonce) > maxNonceLength || len(c.ReviewNonce) > maxNonceLength {
		return fmt.Errorf("%w: the submission nonce must be 1 to %d bytes", ErrInvalidCommand, maxNonceLength)
	}
	if c.Assertion.Challenge.IsZero() || c.Assertion.AssertedAt.IsZero() {
		return fmt.Errorf("%w: the assertion must name its challenge and time", ErrInvalidCommand)
	}
	return nil
}

// Assert completes a governed submission and returns it as it stands now. A
// replay with the same nonce and payload returns the same outcome.
func Assert(ctx context.Context, pool *pgxpool.Pool, cmd AssertSpend) (Submission, error) {
	cmd.Assertion.AssertedAt = cmd.Assertion.AssertedAt.UTC().Truncate(time.Microsecond)
	if err := cmd.validate(); err != nil {
		return Submission{}, err
	}
	digest, err := fingerprint(commandAssertSpend, assertPayload{
		Submission: cmd.SubmissionNonce, Review: cmd.ReviewNonce, ExpectedVersion: cmd.ExpectedVersion, Assertion: cmd.Assertion,
	})
	if err != nil {
		return Submission{}, err
	}
	if _, err := execute(ctx, pool, cmd.Command, commandAssertSpend, digest, cmd.Assertion.AssertedAt, func(u *unit) (json.RawMessage, error) {
		return assertSpend(ctx, u, cmd)
	}); err != nil {
		return Submission{}, err
	}
	return spendScheduling(ctx, pool, cmd)
}

func assertSpend(ctx context.Context, u *unit, cmd AssertSpend) (json.RawMessage, error) {
	principal := cmd.Actor.Principal()
	tenant := principal.Tenant
	submission, err := spendScheduling(ctx, u.uow, cmd)
	if err != nil {
		return nil, err
	}
	if submission.Challenge == nil {
		return nil, fmt.Errorf("%w: the submission is not spend-governed", ErrInvalidCommand)
	}
	if cmd.Assertion.Challenge != submission.Challenge.ID {
		return nil, fmt.Errorf("%w: the assertion names another challenge", ErrInvalidCommand)
	}
	// Serialize with cancellation and competing assertions before inspecting
	// the challenge. Only the command executor may replay a consumed proof:
	// its original nonce has already returned above, without running work.
	if _, _, err := store.LockResultScope(ctx, u.uow, tenant, submission.Attempt.ID); err != nil {
		return nil, err
	}
	challenge, err := store.GetSpendChallengeForUpdate(ctx, u.uow, tenant, submission.Challenge.ID)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err := u.uow.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	u.at = now.UTC().Truncate(time.Microsecond)
	tr, err := u.transition(cmd.ExpectedVersion, nil)
	if err != nil {
		return nil, err
	}
	refusal := func(code lifecycle.RejectionCode) error {
		return &lifecycle.TransitionError{Code: code, Change: lifecycle.Change{
			Tenant: tenant, Record: lifecycle.RecordSpend, RecordID: challenge.ID.Untyped(),
			Event: "assert", From: string(challenge.State), Version: challenge.Version,
		}}
	}
	if challenge.State != lifecycle.SpendPending {
		return nil, refusal(lifecycle.RejectTerminalState)
	}
	if !u.at.Before(challenge.ExpiresAt) {
		return nil, refusal(lifecycle.RejectRequestExpired)
	}

	// Cancellation may have committed while this command waited for scope.
	// Reload scheduling under the lock rather than authorizing its old view.
	submission, err = spendScheduling(ctx, u.uow, cmd)
	if err != nil {
		return nil, err
	}
	if submission.Task.State != lifecycle.TaskQueued || submission.Attempt.State != lifecycle.AttemptAllocated || submission.Operation.State != lifecycle.OperationRecorded {
		return nil, fmt.Errorf("%w: the submission is no longer schedulable", ErrInvalidCommand)
	}
	if submission.Reservation.State != lifecycle.ReservationHeld {
		return nil, fmt.Errorf("%w: the submission's reservation is no longer held for this amount", ErrInvalidCommand)
	}
	brief, present := submission.Task.Brief()
	if !present {
		return nil, fmt.Errorf("%w: legacy task has no execution brief", ErrInvalidCommand)
	}

	// Assertion time remains evidence, never the authorization clock.
	u.lastAt = cmd.Assertion.AssertedAt
	if _, _, _, err := store.AssertSpendChallenge(ctx, u.uow, tenant, submission.Challenge.ID, cmd.Assertion, cmd.ExpectedVersion, tr); err != nil {
		return nil, err
	}
	// Keep proof mismatch validation ahead of the reservation amount check.
	// Any failure rolls the provisional receipt back with this transaction.
	if submission.Reservation.Amount != cmd.Assertion.Terms.Amount {
		return nil, fmt.Errorf("%w: the submission's reservation is no longer held for this amount", ErrInvalidCommand)
	}

	// Only a recorded operation can create dispatch work. Exact command
	// replay bypasses this callback and cannot enqueue another row.
	if err := enqueue(ctx, u, tenant, dispatchSpec{
		operation: submission.Operation, task: submission.Task.ID, attempt: submission.Attempt.ID, demand: submission.Demand,
		deadline: submission.Deadline, unit: submission.Unit, amount: submission.Amount,
		start: contracts.RuntimeStart{Brief: brief},
	}); err != nil {
		return nil, err
	}
	return json.Marshal(assertion{Submission: cmd.SubmissionNonce, Spend: submission.Challenge.ID.String()})
}

func spendScheduling(ctx context.Context, q store.Querier, cmd AssertSpend) (Submission, error) {
	principal := cmd.Actor.Principal()
	kind, nonce := commandSubmitTask, cmd.SubmissionNonce
	if cmd.ReviewNonce != "" {
		kind, nonce = commandReviewResult, cmd.ReviewNonce
	}
	record, err := store.GetCommandRecord(ctx, q, principal.Tenant, principal.ID, kind, nonce)
	if errors.Is(err, store.ErrNotFound) {
		return Submission{}, fmt.Errorf("%w: no such scheduling record", ErrInvalidCommand)
	}
	if err != nil {
		return Submission{}, err
	}
	raw := record.Result
	if cmd.ReviewNonce != "" {
		var v ReviewView
		if err = json.Unmarshal(raw, &v); err != nil {
			return Submission{}, err
		}
		if len(v.Scheduling) == 0 {
			return Submission{}, ErrInvalidCommand
		}
		raw = v.Scheduling
	}
	return loadSubmission(ctx, q, principal.Tenant, nonce, raw)
}
