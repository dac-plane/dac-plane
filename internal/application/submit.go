package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/progress"
)

const commandSubmitTask = "submit_task"

// SubmitTask delegates one Task: it is queued, its first Attempt allocated,
// its Budget created and the Attempt's reservation held, and a runtime
// start_turn operation recorded against the selected harness. An ungoverned
// submission enqueues that operation's dispatch in the same transaction. A
// spend-governed one opens a spend challenge instead and enqueues nothing
// until AssertSpend completes it with a receipt.
type SubmitTask struct {
	Command
	// At is the command time.
	At time.Time
	// Provider selects the harness: it must have a qualification that is not
	// off. No production provider is chosen here.
	Provider ids.ProviderID
	// Demand is the scheduling hint, person or background; empty means
	// background.
	Demand contracts.Demand
	// Deadline bounds the dispatch and the spend challenge; it must be after
	// At.
	Deadline time.Time
	// Unit, Ceiling and Amount create the task budget and size the
	// Attempt's reservation.
	Unit    lifecycle.Unit
	Ceiling uint64
	Amount  uint64
	// Spend makes the submission spend-governed.
	Spend bool
}

// submitPayload is the fingerprinted part of a submission: everything but
// the nonce and the command time.
type submitPayload struct {
	Provider string `json:"provider"`
	Demand   string `json:"demand"`
	Deadline string `json:"deadline"`
	Unit     string `json:"unit"`
	Ceiling  uint64 `json:"ceiling"`
	Amount   uint64 `json:"amount"`
	Spend    bool   `json:"spend"`
}

// submission is the outcome a submit_task record stores: the identities the
// submission created, which later session, turn and environment bindings
// reference, and the selection it bound.
type submission struct {
	Task      string `json:"task"`
	Attempt   string `json:"attempt"`
	Operation string `json:"operation"`
	Budget    string `json:"budget"`
	Provider  string `json:"provider"`
	Mode      string `json:"mode"`
	Demand    string `json:"demand"`
	Deadline  string `json:"deadline"`
	Unit      string `json:"unit"`
	Amount    uint64 `json:"amount"`
	Spend     string `json:"spend,omitempty"`
}

// Submission is a submitted task as it stands now: the records the
// submission created, the harness mode it bound, and the dispatch once one
// is enqueued.
type Submission struct {
	Nonce       string
	Task        lifecycle.Task
	Attempt     lifecycle.Attempt
	Operation   lifecycle.ProviderOperation
	Budget      lifecycle.Budget
	Reservation lifecycle.Reservation
	// Challenge is set for spend-governed submissions; Receipt once asserted.
	Challenge *lifecycle.SpendChallenge
	Receipt   *lifecycle.SpendReceipt
	Provider  ids.ProviderID
	Mode      lifecycle.HarnessMode
	Demand    contracts.Demand
	Deadline  time.Time
	Unit      lifecycle.Unit
	Amount    uint64
	// Dispatch is the operation's durable dispatch, nil until enqueued.
	Dispatch *Dispatch
}

// Dispatch is the state of one operation's outbox row.
type Dispatch struct {
	ID          ids.AnyID
	Status      string
	Demand      contracts.Demand
	Attempts    int
	LastError   string
	ExhaustedAt time.Time
}

func (c SubmitTask) normalize() SubmitTask {
	c.At = c.At.UTC().Truncate(time.Microsecond)
	c.Deadline = c.Deadline.UTC().Truncate(time.Microsecond)
	if c.Demand == "" {
		c.Demand = contracts.DemandBackground
	}
	return c
}

// validate checks what every submit must carry, including replays. The
// deadline is compared with the command time only when new work is created
// (see checkDeadline): At is excluded from the fingerprint so a client can
// recover an uncertain outcome later, even after the deadline has passed.
func (c SubmitTask) validate() error {
	if err := c.Command.validate(true); err != nil {
		return err
	}
	switch {
	case c.At.IsZero() || c.Deadline.IsZero():
		return fmt.Errorf("%w: a command time and a deadline are required", ErrInvalidCommand)
	case c.Provider.IsZero():
		return fmt.Errorf("%w: a provider is required", ErrInvalidCommand)
	case !slices.Contains(contracts.Demands(), c.Demand):
		return fmt.Errorf("%w: unknown demand %q", ErrInvalidCommand, c.Demand)
	case c.Unit.Validate() != nil:
		return fmt.Errorf("%w: malformed budget unit", ErrInvalidCommand)
	case c.Ceiling == 0 || c.Ceiling > lifecycle.MaxAmount || c.Amount == 0 || c.Amount > lifecycle.MaxAmount:
		return fmt.Errorf("%w: ceiling and amount must be positive and bounded", ErrInvalidCommand)
	}
	return nil
}

// dispatchSpec is one runtime start_turn to enqueue for a recorded
// operation: its attempt, the submission's demand and deadline, and the
// budget unit and amount the dispatch loop charges.
type dispatchSpec struct {
	operation lifecycle.ProviderOperation
	task      ids.TaskID
	attempt   ids.AttemptID
	demand    contracts.Demand
	deadline  time.Time
	unit      lifecycle.Unit
	amount    uint64
}

// body is the worker's dispatch envelope for the start_turn.
func (d dispatchSpec) body() ([]byte, error) {
	return json.Marshal(map[string]any{
		"operation": d.operation.ID.String(),
		"role":      contracts.RoleRuntime,
		"method":    contracts.MethodStartTurn,
		"deadline":  d.deadline.Format(time.RFC3339Nano),
		"unit":      d.unit,
		"amount":    d.amount,
	})
}

// dispatchID is the stable identity of an operation's dispatch row: one
// operation has one dispatch, so a retried enqueue reuses it and the
// submission view finds it.
func dispatchID(operation ids.OperationID) ids.AnyID {
	return operation.Untyped()
}

// Submit runs a submission. A replay with the same nonce and payload
// returns the original submission as it stands now.
func Submit(ctx context.Context, pool *pgxpool.Pool, cmd SubmitTask) (Submission, error) {
	cmd = cmd.normalize()
	if err := cmd.validate(); err != nil {
		return Submission{}, err
	}
	digest, err := fingerprint(commandSubmitTask, submitPayload{
		Provider: cmd.Provider.String(), Demand: string(cmd.Demand), Deadline: cmd.Deadline.Format(time.RFC3339Nano),
		Unit: string(cmd.Unit), Ceiling: cmd.Ceiling, Amount: cmd.Amount, Spend: cmd.Spend,
	})
	if err != nil {
		return Submission{}, err
	}
	if _, err := execute(ctx, pool, cmd.Command, commandSubmitTask, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		return submit(ctx, u, cmd)
	}); err != nil {
		return Submission{}, err
	}
	return GetSubmission(ctx, pool, cmd.Actor, cmd.Nonce)
}

// checkDeadline rejects new work whose deadline is not after the command
// time. It runs inside the command after replays were resolved, so a late
// retry of a committed submission still returns that submission.
func (c SubmitTask) checkDeadline() error {
	if !c.Deadline.After(c.At) {
		return fmt.Errorf("%w: the deadline must be after the command time", ErrInvalidCommand)
	}
	return nil
}

func submit(ctx context.Context, u *unit, cmd SubmitTask) (json.RawMessage, error) {
	if err := cmd.checkDeadline(); err != nil {
		return nil, err
	}
	principal := cmd.Actor.Principal()
	tenant := principal.Tenant

	harness, err := store.GetHarness(ctx, u.uow, tenant, cmd.Provider)
	if errors.Is(err, store.ErrNotFound) || (err == nil && harness.Mode == lifecycle.HarnessOff) {
		return nil, ErrNoHarness
	}
	if err != nil {
		return nil, err
	}

	taskID, err := ids.New[ids.Task]()
	if err != nil {
		return nil, err
	}
	task, err := lifecycle.NewTask(taskID, tenant, principal.ID, cmd.At)
	if err != nil {
		return nil, err
	}
	if _, _, err := store.InsertTask(ctx, u.uow, task); err != nil {
		return nil, err
	}
	tr, err := u.transition(task.Version, nil)
	if err != nil {
		return nil, err
	}
	if _, _, err := store.ApplyTask(ctx, u.uow, tenant, task.ID, lifecycle.TaskSubmit, "", tr); err != nil {
		return nil, err
	}

	attemptID, err := ids.New[ids.Attempt]()
	if err != nil {
		return nil, err
	}
	attempt, err := lifecycle.NewAttempt(attemptID, tenant, task.ID, 1, cmd.At)
	if err != nil {
		return nil, err
	}
	if _, _, err := store.InsertAttempt(ctx, u.uow, attempt); err != nil {
		return nil, err
	}

	budgetID, err := ids.New[ids.Budget]()
	if err != nil {
		return nil, err
	}
	budget, err := lifecycle.NewBudget(budgetID, tenant, task.ID, cmd.Unit, cmd.Ceiling, cmd.At)
	if err != nil {
		return nil, err
	}
	if _, _, err = store.InsertBudget(ctx, u.uow, budget); err != nil {
		return nil, err
	}
	result, err := scheduleAttempt(ctx, u, task, attempt, budget, cmd, harness, commandSubmitTask)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// scheduleAttempt is shared by first submission and request_changes. It
// records the reservation, operation, spend challenge and dispatch atomically;
// callers supply the existing Task/Budget rather than creating another Task.
func scheduleAttempt(ctx context.Context, u *unit, task lifecycle.Task, attempt lifecycle.Attempt, budget lifecycle.Budget, cmd SubmitTask, harness lifecycle.HarnessQualification, kind string) (submission, error) {
	principal := cmd.Actor.Principal()
	tenant := task.Tenant
	var tr store.Transition
	// Reserve before anything dispatchable exists. The reservation identity
	// derives from the Attempt, as the dispatch loop's budget gate expects,
	// so its later check finds this hold instead of taking a second one.
	reservationID, err := ids.Parse[ids.Reservation](attempt.ID.String())
	if err != nil {
		return submission{}, err
	}
	if tr, err = u.transition(budget.Version, nil); err != nil {
		return submission{}, err
	}
	if _, _, _, err := store.ReserveBudget(ctx, u.uow, store.ReservationRequest{
		Tenant: tenant, Budget: budget.ID, Reservation: reservationID, Attempt: attempt.ID, Amount: cmd.Amount,
	}, tr); err != nil {
		return submission{}, err
	}

	operationID, err := ids.New[ids.Operation]()
	if err != nil {
		return submission{}, err
	}
	operation, err := lifecycle.NewProviderOperation(operationID, tenant, attempt.ID, cmd.Provider, cmd.At)
	if err != nil {
		return submission{}, err
	}
	if _, _, err := store.InsertOperation(ctx, u.uow, operation); err != nil {
		return submission{}, err
	}

	result := submission{
		Task: task.ID.String(), Attempt: attempt.ID.String(), Operation: operation.ID.String(),
		Budget: budget.ID.String(), Provider: cmd.Provider.String(), Mode: string(harness.Mode),
		Demand: string(cmd.Demand), Deadline: cmd.Deadline.Format(time.RFC3339Nano),
		Unit: string(cmd.Unit), Amount: cmd.Amount,
	}
	if cmd.Spend {
		// Governed: open the challenge and enqueue nothing. The dispatch
		// commits later, together with the receipt.
		spendID, err := ids.New[ids.Spend]()
		if err != nil {
			return submission{}, err
		}
		challenge, err := lifecycle.BeginSpend(spendID, lifecycle.SpendTerms{
			Tenant: tenant, Principal: principal.ID, Task: task.ID, Attempt: attempt.ID,
			Operation: operation.ID, Amount: cmd.Amount, Unit: cmd.Unit,
			Nonce: identity.DigestOf([]byte(kind + ":" + tenant.String() + ":" + principal.ID.String() + ":" + cmd.Nonce)),
		}, cmd.Deadline, cmd.At)
		if err != nil {
			return submission{}, err
		}
		if _, _, err := store.InsertSpendChallenge(ctx, u.uow, challenge); err != nil {
			return submission{}, err
		}
		result.Spend = spendID.String()
	} else if err := enqueue(ctx, u, tenant, dispatchSpec{
		operation: operation, task: task.ID, attempt: attempt.ID, demand: cmd.Demand,
		deadline: cmd.Deadline, unit: cmd.Unit, amount: cmd.Amount,
	}); err != nil {
		return submission{}, err
	}
	return result, nil
}

// enqueue moves the recorded operation to dispatched together with its
// outbox row, and moves the Task and Attempt into their dispatched states. The row names the operation as its record, which is how the
// dispatch loop finds what to send. A replay of an already-dispatched
// operation writes nothing.
func enqueue(ctx context.Context, u *unit, tenant ids.TenantID, d dispatchSpec) error {
	body, err := d.body()
	if err != nil {
		return err
	}
	tr, err := u.transition(d.operation.Version, &store.OutboxPayload{
		ID: dispatchID(d.operation.ID), Attempt: d.attempt, Demand: d.demand, Body: body,
	})
	if err != nil {
		return err
	}
	if _, _, err = store.ApplyOperation(ctx, u.uow, tenant, d.operation.ID, lifecycle.OperationDispatch, lifecycle.OperationDispatched, tr); err != nil {
		return err
	}
	// Once a dispatch may be sent, execution may be live: the Task runs and
	// the Attempt leaves allocated, so a cancel waits for stop observations
	// instead of completing at once.
	step := progress.Step{UOW: u.uow, Tenant: tenant, Actor: u.actor, At: u.at}
	if _, err := step.Task(ctx, d.task, lifecycle.TaskDispatch); err != nil {
		return err
	}
	_, _, _, err = step.Attempt(ctx, d.attempt, lifecycle.AttemptProvision, "")
	return err
}

// GetSubmission loads the actor's submission recorded under nonce, with the
// current state of its records and its dispatch. Other tenants' and
// principals' submissions read as not found.
func GetSubmission(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, nonce string) (Submission, error) {
	principal := actor.Principal()
	if principal.Validate() != nil {
		return Submission{}, fmt.Errorf("%w: an authenticated principal is required", ErrInvalidCommand)
	}
	record, err := store.GetCommandRecord(ctx, pool, principal.Tenant, principal.ID, commandSubmitTask, nonce)
	if err != nil {
		return Submission{}, err
	}
	return loadSubmission(ctx, pool, principal.Tenant, nonce, record.Result)
}

func loadSubmission(ctx context.Context, q store.Querier, tenant ids.TenantID, nonce string, raw json.RawMessage) (Submission, error) {
	var facts submission
	if err := json.Unmarshal(raw, &facts); err != nil {
		return Submission{}, fmt.Errorf("submission record: %w", err)
	}
	s := Submission{
		Nonce: nonce, Mode: lifecycle.HarnessMode(facts.Mode), Demand: contracts.Demand(facts.Demand),
		Unit: lifecycle.Unit(facts.Unit), Amount: facts.Amount,
	}
	var err error
	if s.Deadline, err = time.Parse(time.RFC3339Nano, facts.Deadline); err != nil {
		return Submission{}, fmt.Errorf("submission deadline: %w", err)
	}
	taskID, err := ids.Parse[ids.Task](facts.Task)
	if err != nil {
		return Submission{}, err
	}
	attemptID, err := ids.Parse[ids.Attempt](facts.Attempt)
	if err != nil {
		return Submission{}, err
	}
	operationID, err := ids.Parse[ids.Operation](facts.Operation)
	if err != nil {
		return Submission{}, err
	}
	budgetID, err := ids.Parse[ids.Budget](facts.Budget)
	if err != nil {
		return Submission{}, err
	}
	if s.Provider, err = ids.Parse[ids.Provider](facts.Provider); err != nil {
		return Submission{}, err
	}
	reservationID, err := ids.Parse[ids.Reservation](facts.Attempt)
	if err != nil {
		return Submission{}, err
	}
	if s.Task, err = store.GetTask(ctx, q, tenant, taskID); err != nil {
		return Submission{}, err
	}
	if s.Attempt, err = store.GetAttempt(ctx, q, tenant, attemptID); err != nil {
		return Submission{}, err
	}
	if s.Operation, err = store.GetOperation(ctx, q, tenant, operationID); err != nil {
		return Submission{}, err
	}
	if s.Budget, err = store.GetBudget(ctx, q, tenant, budgetID); err != nil {
		return Submission{}, err
	}
	if s.Reservation, err = store.GetReservation(ctx, q, tenant, reservationID); err != nil {
		return Submission{}, err
	}
	if facts.Spend != "" {
		spendID, err := ids.Parse[ids.Spend](facts.Spend)
		if err != nil {
			return Submission{}, err
		}
		challenge, err := store.GetSpendChallenge(ctx, q, tenant, spendID)
		if err != nil {
			return Submission{}, err
		}
		s.Challenge = &challenge
		receipt, err := store.GetSpendReceipt(ctx, q, tenant, spendID)
		if err == nil {
			s.Receipt = &receipt
		} else if !errors.Is(err, store.ErrNotFound) {
			return Submission{}, err
		}
	}
	row, err := store.GetOutboxRow(ctx, q, tenant, dispatchID(operationID))
	if err == nil {
		s.Dispatch = &Dispatch{
			ID: row.ID, Status: row.Status, Demand: row.Demand, Attempts: row.Attempts,
			LastError: row.LastError, ExhaustedAt: row.ExhaustedAt,
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return Submission{}, err
	}
	return s, nil
}
