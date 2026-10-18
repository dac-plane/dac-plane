package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	Assertion       lifecycle.SpendAssertion
}

type assertPayload struct {
	Submission      string                   `json:"submission"`
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
	if c.SubmissionNonce == "" || len(c.SubmissionNonce) > maxNonceLength {
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
		Submission: cmd.SubmissionNonce, ExpectedVersion: cmd.ExpectedVersion, Assertion: cmd.Assertion,
	})
	if err != nil {
		return Submission{}, err
	}
	if _, err := execute(ctx, pool, cmd.Command, commandAssertSpend, digest, cmd.Assertion.AssertedAt, func(u *unit) (json.RawMessage, error) {
		return assertSpend(ctx, u, cmd)
	}); err != nil {
		return Submission{}, err
	}
	return GetSubmission(ctx, pool, cmd.Actor, cmd.SubmissionNonce)
}

func assertSpend(ctx context.Context, u *unit, cmd AssertSpend) (json.RawMessage, error) {
	principal := cmd.Actor.Principal()
	tenant := principal.Tenant
	record, err := store.GetCommandRecord(ctx, u.uow, tenant, principal.ID, commandSubmitTask, cmd.SubmissionNonce)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: no such submission", ErrInvalidCommand)
	}
	if err != nil {
		return nil, err
	}
	submission, err := loadSubmission(ctx, u.uow, tenant, cmd.SubmissionNonce, record.Result)
	if err != nil {
		return nil, err
	}
	if submission.Challenge == nil {
		return nil, fmt.Errorf("%w: the submission is not spend-governed", ErrInvalidCommand)
	}
	if cmd.Assertion.Challenge != submission.Challenge.ID {
		return nil, fmt.Errorf("%w: the assertion names another challenge", ErrInvalidCommand)
	}

	// The store audits an assertion at its assertion time.
	tr, err := u.transition(cmd.ExpectedVersion, nil)
	if err != nil {
		return nil, err
	}
	u.lastAt = cmd.Assertion.AssertedAt
	if _, _, _, err := store.AssertSpendChallenge(ctx, u.uow, tenant, submission.Challenge.ID, cmd.Assertion, cmd.ExpectedVersion, tr); err != nil {
		return nil, err
	}

	// Receipt and reservation are both required before enqueue.
	if submission.Reservation.State != lifecycle.ReservationHeld || submission.Reservation.Amount != cmd.Assertion.Terms.Amount {
		return nil, fmt.Errorf("%w: the submission's reservation is no longer held for this amount", ErrInvalidCommand)
	}
	// An assertion replayed after its dispatch was enqueued converges: the
	// operation already left recorded, so nothing is enqueued twice.
	if submission.Operation.State == lifecycle.OperationRecorded {
		if err := enqueue(ctx, u, tenant, dispatchSpec{
			operation: submission.Operation, attempt: submission.Attempt.ID, demand: submission.Demand,
			deadline: submission.Deadline, unit: submission.Unit, amount: submission.Amount,
		}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(assertion{Submission: cmd.SubmissionNonce, Spend: submission.Challenge.ID.String()})
}
