package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GovernedActionRequest proposes one action from an authenticated provider.
// Input travels only to the pre-action hook. M1 executes a fixed resource.mutate
// simulator action; the descriptor binds the resource and value by exact digest.
type GovernedActionRequest struct {
	Request      identity.ActionRequest
	Grant        ids.GrantID
	Input        json.RawMessage
	Precondition *contracts.ActionPrecondition
	Deadline     time.Time
}

type GovernedActionView = store.GovernedAction

// EnforceGovernedAction evaluates interception outside the transaction, then
// revalidates current authority and commits its use and dispatch together.
// peer is authenticated by the caller's transport, never taken from the input.
func EnforceGovernedAction(ctx context.Context, pool *pgxpool.Pool, peer contracts.Peer, port contracts.Port, cmd GovernedActionRequest, at time.Time) (GovernedActionView, error) {
	at = at.UTC().Truncate(time.Microsecond)
	if at.IsZero() || cmd.Deadline.IsZero() || cmd.Request.Validate() != nil || len(cmd.Input) == 0 || identity.DigestOf(cmd.Input) != cmd.Request.InputDigest {
		return GovernedActionView{}, ErrInvalidCommand
	}
	if peer.Tenant.IsZero() || peer.Provider.IsZero() {
		return GovernedActionView{}, &contracts.Failure{Code: contracts.FailureUnauthenticated, Message: "authenticated provider required"}
	}
	if cmd.Request.Tenant != peer.Tenant || cmd.Request.Provider != peer.Provider {
		return GovernedActionView{}, &contracts.Failure{Code: contracts.FailureUnauthorized, Message: "action does not belong to authenticated provider"}
	}
	if err := checkResourceInput(cmd.Request, cmd.Input, cmd.Precondition); err != nil {
		return GovernedActionView{}, err
	}
	digest, err := fingerprint("governed_action", struct {
		Request      identity.ActionRequest
		Grant        ids.GrantID `json:",omitzero"`
		Precondition *contracts.ActionPrecondition
		Deadline     time.Time
	}{cmd.Request, cmd.Grant, cmd.Precondition, cmd.Deadline})
	if err != nil {
		return GovernedActionView{}, err
	}
	if stored, err := store.GetGovernedAction(ctx, pool, peer.Tenant, cmd.Request.Operation); err == nil {
		if stored.Digest != digest {
			return GovernedActionView{}, ErrPayloadMismatch
		}
		return stored, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return GovernedActionView{}, err
	}
	if err = checkActionAttempt(ctx, pool, cmd.Request); err != nil {
		return GovernedActionView{}, err
	}
	hook := contracts.HookRequest{Kind: contracts.HookPreAction, Operation: cmd.Request.Operation, Tenant: peer.Tenant, Attempt: cmd.Request.Attempt, Provider: peer.Provider, Action: cmd.Request.Action, Scope: cmd.Request.Scope, Input: cmd.Input, InputDigest: cmd.Request.InputDigest, Deadline: cmd.Deadline}
	decision, err := evaluateActionHook(ctx, port, hook, at)
	if err != nil {
		return GovernedActionView{}, err
	}
	effective := cmd.Request
	if decision.Allowed() {
		effective = decision.EffectiveRequest(effective)
		if err := checkResourceInput(effective, decision.EffectiveInput, cmd.Precondition); err != nil {
			// A rewrite changing the resource is not the approved action.
			decision.Evidence.Outcome = contracts.HookMalformed
			decision.Evidence.EffectiveDigest = ""
			decision.Evidence.Rewritten = false
			effective = cmd.Request
		}
	}
	u, err := store.Begin(ctx, pool)
	if err != nil {
		return GovernedActionView{}, err
	}
	defer func() { _ = u.Rollback(ctx) }()
	if err = store.LockGovernedAction(ctx, u, peer.Tenant, cmd.Request.Operation); err != nil {
		return GovernedActionView{}, err
	}
	if stored, err := store.GetGovernedAction(ctx, u, peer.Tenant, cmd.Request.Operation); err == nil {
		if stored.Digest != digest {
			return GovernedActionView{}, ErrPayloadMismatch
		}
		return stored, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return GovernedActionView{}, err
	}
	policy, err := store.GetPolicyForUpdate(ctx, u, peer.Tenant)
	if err != nil {
		return GovernedActionView{}, err
	}
	if _, err := store.GetOperation(ctx, u, peer.Tenant, cmd.Request.Operation); err == nil {
		return GovernedActionView{}, ErrPayloadMismatch
	} else if !errors.Is(err, store.ErrNotFound) {
		return GovernedActionView{}, err
	}
	if _, err = store.GetAttemptForUpdate(ctx, u, peer.Tenant, cmd.Request.Attempt); err != nil {
		return GovernedActionView{}, err
	}
	var grant *identity.AuthorityGrant
	if !cmd.Grant.IsZero() {
		g, e := store.GetGrantForUpdate(ctx, u, peer.Tenant, cmd.Grant)
		if e == nil {
			grant = &g
		} else if !errors.Is(e, store.ErrNotFound) {
			return GovernedActionView{}, e
		}
	}
	// Cancel and issuance use this lock too: no action can be authorized once
	// cancellation has committed, including while the hook was outstanding.
	if err = checkActionAttempt(ctx, u, cmd.Request); err != nil {
		return GovernedActionView{}, err
	}
	// Authorization uses current server time, never a provider-supplied timestamp.
	decidedAt := time.Now().UTC().Truncate(time.Microsecond)
	use, retired, err := identity.Authorize(effective, grant, policy, decidedAt)
	if err != nil {
		return GovernedActionView{}, err
	}
	if !decision.Allowed() {
		use, err = identity.DenyAfterHook(effective, grant, policy, decidedAt)
		if err != nil {
			return GovernedActionView{}, err
		}
		retired = nil
	}
	if use.Outcome == identity.UseAllowed && (grant.Decision.IsZero()) {
		return GovernedActionView{}, fmt.Errorf("%w: grant has no recorded approval", ErrInvalidCommand)
	}
	execution := contracts.ActionExecution{Request: effective, Grant: use.Grant, Decision: use.Decision, Source: use.Source, Precondition: cmd.Precondition}
	view := store.GovernedAction{Digest: digest, Execution: execution, Use: use, Hook: decision.Evidence}
	audit, err := ids.New[ids.Audit]()
	if err != nil {
		return view, err
	}
	tr := store.Transition{At: decidedAt, AuditID: audit, Actor: lifecycle.Actor{Kind: lifecycle.ActorProvider, Provider: peer.Provider}}
	if use.Outcome == identity.UseAllowed {
		if retired != nil {
			if _, _, err = store.RetireGrant(ctx, u, peer.Tenant, retired.ID, decidedAt); err != nil {
				return view, err
			}
			retireAudit, err := ids.New[ids.Audit]()
			if err != nil {
				return view, err
			}
			if err = store.RecordAuthorityChange(ctx, u, *retired, "retire", store.Transition{At: decidedAt, AuditID: retireAudit, Actor: tr.Actor}); err != nil {
				return view, err
			}
		}
		op, e := lifecycle.NewProviderOperation(cmd.Request.Operation, peer.Tenant, cmd.Request.Attempt, peer.Provider, decidedAt)
		if e != nil {
			return view, e
		}
		if _, _, err = store.InsertOperation(ctx, u, op); err != nil {
			return view, err
		}
		body, e := json.Marshal(struct {
			Operation ids.OperationID  `json:"operation"`
			Role      contracts.Role   `json:"role"`
			Method    contracts.Method `json:"method"`
			Deadline  time.Time        `json:"deadline"`
		}{op.ID, contracts.RoleEnforcement, contracts.MethodExecuteAction, cmd.Deadline})
		if e != nil {
			return view, e
		}
		tr.Outbox = &store.OutboxPayload{ID: op.ID.Untyped(), Attempt: op.Attempt, Demand: contracts.DemandPerson, Body: body}
	}
	if err = store.InsertGovernedAction(ctx, u, view, tr); err != nil {
		return view, err
	}
	if err = u.Commit(ctx); err != nil {
		return view, err
	}
	return view, nil
}

func checkResourceInput(req identity.ActionRequest, input json.RawMessage, p *contracts.ActionPrecondition) error {
	var value struct {
		Resource string          `json:"resource"`
		Value    json.RawMessage `json:"value"`
	}
	if len(input) > 32<<10 || json.Unmarshal(input, &value) != nil || req.Action != "resource.mutate" || req.Scope.Class != identity.ScopeSubject || req.Scope.Target != value.Resource || len(value.Value) == 0 || p == nil || p.Validate() != nil || p.Resource != value.Resource || p.Operation != req.Operation {
		return fmt.Errorf("%w: resource.mutate requires matching subject, resource input and observation precondition", ErrInvalidCommand)
	}
	return nil
}

func checkActionAttempt(ctx context.Context, q store.Querier, r identity.ActionRequest) error {
	attempt, err := store.GetAttempt(ctx, q, r.Tenant, r.Attempt)
	if err != nil {
		return err
	}
	if attempt.State != lifecycle.AttemptExecuting && attempt.State != lifecycle.AttemptWaitingInput {
		return &contracts.Failure{Code: contracts.FailureCancelled, Message: "attempt cannot perform governed actions"}
	}
	runtime, err := store.GetOperationByAttempt(ctx, q, r.Tenant, r.Attempt)
	if err != nil {
		return err
	}
	if runtime.Provider != r.Provider {
		return &contracts.Failure{Code: contracts.FailureUnauthorized, Message: "provider does not own attempt"}
	}
	h, err := store.GetHarness(ctx, q, r.Tenant, r.Provider)
	if err != nil {
		return err
	}
	if h.Mode != lifecycle.HarnessLive {
		return ErrNoHarness
	}
	return nil
}

func evaluateActionHook(ctx context.Context, port contracts.Port, hook contracts.HookRequest, at time.Time) (contracts.HookDecision, error) {
	if err := hook.Validate(); err != nil {
		return contracts.HookDecision{}, err
	}
	base, _ := contracts.DecidePreAction(hook, nil, at)
	if port == nil || port.Role() != contracts.RoleEnforcement {
		return base, nil
	}
	if !time.Now().Before(hook.Deadline) {
		base.Evidence.Outcome = contracts.HookTimedOut
		return base, nil
	}
	delivery, err := ids.New[ids.Delivery]()
	if err != nil {
		return base, err
	}
	// RawMessage's normal marshaler compacts JSON. Preserve the action bytes
	// exactly: approval digests deliberately do not canonicalize JSON.
	input := hook.Input
	wireHook := hook
	wireHook.Input = nil
	raw, err := json.Marshal(wireHook)
	if err != nil {
		return base, err
	}
	raw = append(raw[:len(raw)-1], []byte(`,"input":`)...)
	raw = append(raw, input...)
	raw = append(raw, '}')
	sctx, cancel := context.WithDeadline(ctx, hook.Deadline)
	defer cancel()
	reply, err := port.Send(sctx, contracts.Request{Operation: hook.Operation, Delivery: delivery, Tenant: hook.Tenant, Attempt: hook.Attempt, Role: contracts.RoleEnforcement, Method: contracts.MethodPreAction, Arguments: raw, Demand: contracts.DemandPerson, Deadline: hook.Deadline})
	if err != nil {
		base.Evidence.Outcome = contracts.HookUnavailable
		if sctx.Err() != nil {
			base.Evidence.Outcome = contracts.HookTimedOut
		}
		return base, nil
	}
	var response contracts.HookResponse
	decoder := json.NewDecoder(bytes.NewReader(reply.Result))
	decoder.DisallowUnknownFields()
	if reply.Validate() != nil || reply.Operation != hook.Operation || reply.Delivery != delivery || reply.Durability != lifecycle.SendAccepted || decoder.Decode(&response) != nil {
		base.Evidence.Outcome = contracts.HookMalformed
		return base, nil
	}
	return contracts.DecidePreAction(hook, &response, time.Now().UTC())
}

// GetGovernedAction reads evidence only for the owner of the task.
func GetGovernedAction(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, operation ids.OperationID) (GovernedActionView, error) {
	a, err := store.GetGovernedAction(ctx, pool, actor.Principal().Tenant, operation)
	if err != nil {
		return a, err
	}
	attempt, err := store.GetAttempt(ctx, pool, a.Use.Tenant, a.Use.Attempt)
	if err != nil {
		return a, err
	}
	task, err := store.GetTask(ctx, pool, a.Use.Tenant, attempt.Task)
	if err != nil {
		return a, err
	}
	if task.Owner != actor.Principal().ID {
		return GovernedActionView{}, ErrNotFound
	}
	return a, nil
}

// RevokeAuthority is an owner-authorized, idempotent standing grant revocation.
type RevokeAuthority struct {
	Command
	Grant ids.GrantID
	At    time.Time
}

func Revoke(ctx context.Context, pool *pgxpool.Pool, cmd RevokeAuthority) error {
	if err := cmd.Command.validate(true); err != nil {
		return err
	}
	if cmd.Grant.IsZero() || cmd.At.IsZero() {
		return ErrInvalidCommand
	}
	digest, err := fingerprint("revoke_authority", cmd.Grant)
	if err != nil {
		return err
	}
	_, err = execute(ctx, pool, cmd.Command, "revoke_authority", digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		g, err := store.GetGrantForUpdate(ctx, u.uow, cmd.Actor.Principal().Tenant, cmd.Grant)
		if err != nil {
			return nil, err
		}
		a, err := store.GetAttempt(ctx, u.uow, g.Tenant, g.Attempt)
		if err != nil {
			return nil, err
		}
		task, err := store.GetTask(ctx, u.uow, g.Tenant, a.Task)
		if err != nil {
			return nil, err
		}
		if task.Owner != cmd.Actor.Principal().ID {
			return nil, ErrUnauthorized
		}
		if g.Mode != identity.GrantStanding {
			return nil, ErrInvalidCommand
		}
		_, replayed, err := store.RevokeGrant(ctx, u.uow, g.Tenant, g.ID, cmd.At)
		if err != nil {
			return nil, err
		}
		if !replayed {
			tr, err := u.transition(1, nil)
			if err != nil {
				return nil, err
			}
			if err = store.RecordAuthorityChange(ctx, u.uow, g, "revoke", tr); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{"revoked":true}`), nil
	})
	return err
}
