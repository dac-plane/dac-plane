// Package application The decision command records a responder's answer to an attention request
// under the frozen Decide rules, issues an approval's scoped grant from the
// correlation row's action class, and enqueues the decision's delivery to the
// runtime — all in one transaction the idempotency ledger replays.
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
)

const commandDecideAttention = "decide_attention"

// DecideAttention answers one version of an attention request. ExpectedVersion
// is the request version the decision was made on: the first valid decision
// for that version wins, and every later or stale decision is refused with
// evidence.
type DecideAttention struct {
	Command
	// Attention is the request being answered.
	Attention ids.AttentionID
	// Kind is the decision: approve_once for an action approval, deny for
	// any request, or answer with the content digest of a question.
	Kind lifecycle.DecisionKind
	// AnswerDigest is the digest of the answer's content, carried by an
	// answer decision only.
	AnswerDigest identity.Digest
	// At is the decision time; it must not be after the request's expiry.
	At time.Time
}

func (c DecideAttention) validate() error {
	if err := c.Command.validate(false); err != nil {
		return err
	}
	if c.At.IsZero() {
		return fmt.Errorf("%w: a decision time is required", ErrInvalidCommand)
	}
	if c.Attention.IsZero() {
		return fmt.Errorf("%w: an attention request is required", ErrInvalidCommand)
	}
	if !slices.Contains(lifecycle.DecisionKinds(), c.Kind) {
		return fmt.Errorf("%w: unknown decision kind", ErrInvalidCommand)
	}
	if c.Kind == lifecycle.DecisionAnswer {
		if err := c.AnswerDigest.Validate(); err != nil {
			return fmt.Errorf("%w: an answer carries its content digest: %w", ErrInvalidCommand, err)
		}
		return nil
	}
	if c.AnswerDigest != "" {
		return fmt.Errorf("%w: only an answer carries an answer digest", ErrInvalidCommand)
	}
	return nil
}

// DecisionView is the result of a decided request: the recorded decision and
// state, and the grant an approval issued.
type DecisionView struct {
	Nonce       string
	AttentionID string
	DecisionID  string
	Kind        string
	State       string
	GrantID     string
}

type decidePayload struct {
	Attention       string `json:"attention"`
	Kind            string `json:"kind"`
	AnswerDigest    string `json:"answer_digest"`
	ExpectedVersion uint64 `json:"expected_version"`
}

// Decide runs the decision. A replay with the same nonce and payload
// returns the recorded decision.
func Decide(ctx context.Context, pool *pgxpool.Pool, cmd DecideAttention) (DecisionView, error) {
	if err := cmd.validate(); err != nil {
		return DecisionView{}, err
	}
	digest, err := fingerprint(commandDecideAttention, decidePayload{
		Attention: cmd.Attention.String(), Kind: string(cmd.Kind),
		AnswerDigest: string(cmd.AnswerDigest), ExpectedVersion: cmd.ExpectedVersion,
	})
	if err != nil {
		return DecisionView{}, err
	}
	if _, err := execute(ctx, pool, cmd.Command, commandDecideAttention, digest, cmd.At, func(u *unit) (json.RawMessage, error) {
		return decide(ctx, u, cmd)
	}); err != nil {
		return DecisionView{}, err
	}
	record, err := store.GetCommandRecord(ctx, pool, cmd.Actor.Principal().Tenant, cmd.Actor.Principal().ID, commandDecideAttention, cmd.Nonce)
	if err != nil {
		return DecisionView{}, err
	}
	var view DecisionView
	if err := json.Unmarshal(record.Result, &view); err != nil {
		return DecisionView{}, fmt.Errorf("decision record: %w", err)
	}
	view.Nonce = cmd.Nonce
	return view, nil
}

// decisionDelivery is the outbox body of a decision's delivery row: what the
// worker needs to steer the runtime without re-deriving anything, and what
// the runtime surfaces at the native request.
type decisionDelivery struct {
	Operation    string               `json:"operation,omitempty"`
	GrantID      string               `json:"grant_id,omitempty"`
	Scope        identity.Scope       `json:"scope,omitzero"`
	DecisionID   string               `json:"decision_id"`
	Attention    string               `json:"attention"`
	Attempt      string               `json:"attempt"`
	NativeRef    string               `json:"native_ref"`
	Kind         string               `json:"kind"`
	AnswerDigest identity.Digest      `json:"answer_digest,omitempty"`
	Action       identity.ActionClass `json:"action,omitempty"`
	ExpiresAt    time.Time            `json:"expires_at"`
}

// decide records the decision: the decision row, the request's move to
// decided, an approval's grant and the delivery row commit together.
func decide(ctx context.Context, u *unit, cmd DecideAttention) (json.RawMessage, error) {
	tenant := cmd.Actor.Principal().Tenant
	// The request is loaded in the actor's own tenant only: another tenant's
	// request reads as absent.
	request, err := store.GetAttention(ctx, u.uow, tenant, cmd.Attention)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// The correlation row carries the native ref the delivery steers to and,
	// for an approval, the action class the observation named: the provenance
	// a responder never supplies.
	correlation, err := store.GetAttentionNativeForAttention(ctx, u.uow, tenant, request.ID)
	if err != nil {
		return nil, err
	}
	policy, err := store.GetPolicyForUpdate(ctx, u.uow, tenant)
	if err != nil {
		return nil, err
	}

	decisionID, err := ids.New[ids.Decision]()
	if err != nil {
		return nil, err
	}
	decision := lifecycle.Decision{
		ID: decisionID, Tenant: tenant, Request: request.ID,
		RequestVersion: cmd.ExpectedVersion, Principal: cmd.Actor.Principal().ID,
		Kind: cmd.Kind, AnswerDigest: cmd.AnswerDigest,
		DecidedAt: cmd.At.UTC().Truncate(time.Microsecond),
	}
	body, err := json.Marshal(decisionDelivery{
		DecisionID: decision.ID.String(), Attention: request.ID.String(),
		Attempt: request.Attempt.String(), NativeRef: correlation.NativeRef,
		Kind: string(decision.Kind), AnswerDigest: decision.AnswerDigest,
		Action: correlation.ActionClass, ExpiresAt: request.ExpiresAt,
	})
	if err != nil {
		return nil, err
	}
	tr, err := u.transition(request.Version, &store.OutboxPayload{
		// The row's identity is the decision's, so a replayed decide never
		// enqueues a second delivery.
		ID: decision.ID.Untyped(), Attempt: request.Attempt,
		Demand: contracts.DemandPerson, Body: body,
	})
	if err != nil {
		return nil, err
	}
	decided, recorded, replayed, err := store.DecideAttention(ctx, u.uow, decision, policy.Generation, tr)
	if err != nil {
		return nil, err
	}
	if replayed && recorded.ID != decision.ID {
		// A racing decision won on the same request version: this command
		// decided on a stale view, so it is a refusal, not the other's
		// replay.
		return nil, &lifecycle.TransitionError{Code: lifecycle.RejectVersionConflict, Change: lifecycle.Change{
			Tenant: tenant, Record: lifecycle.RecordAttention, RecordID: request.ID.Untyped(),
			Event: "decide", From: string(request.State), Version: request.Version,
		}}
	}

	var view DecisionView
	view.AttentionID = request.ID.String()
	view.DecisionID = recorded.ID.String()
	view.Kind = string(recorded.Kind)
	view.State = string(decided.State)
	if (cmd.Kind == lifecycle.DecisionApproveOnce || cmd.Kind == lifecycle.DecisionApproveStanding) && !replayed {
		// An approval confers authority derived from the request
		// and the correlation row, never chosen by the responder; the grant
		// expires exactly at the request's expiry.
		grantID, err := ids.New[ids.Grant]()
		if err != nil {
			return nil, err
		}
		grant, err := decided.AllowOnceGrant(recorded, grantID, correlation.ActionClass, request.ExpiresAt)
		if cmd.Kind == lifecycle.DecisionApproveStanding {
			grant, err = decided.StandingGrant(recorded, grantID, correlation.ActionClass, request.ExpiresAt)
		}
		if err != nil {
			return nil, err
		}
		if err := identity.IssueGrantForScope(grant, request.Scope, policy); err != nil {
			return nil, errors.Join(err, &lifecycle.TransitionError{Code: lifecycle.RejectDecisionNotAllowed, Change: lifecycle.Change{Tenant: tenant, Record: lifecycle.RecordAttention, RecordID: request.ID.Untyped(), Event: "decide", From: string(request.State), Version: request.Version}})
		}
		if err := store.InsertGrant(ctx, u.uow, grant); err != nil {
			return nil, err
		}
		grantTr, err := u.transition(1, nil)
		if err != nil {
			return nil, err
		}
		if err := store.RecordAuthorityChange(ctx, u.uow, grant, "issue", grantTr); err != nil {
			return nil, err
		}
		view.GrantID = grant.ID.String()
	}
	// Approval delivery carries the child action identity and its authority;
	// the runtime-turn operation still routes the native steer request.
	if request.Kind == lifecycle.AttentionActionApproval {
		body, err = json.Marshal(decisionDelivery{DecisionID: recorded.ID.String(), Attention: request.ID.String(),
			Attempt: request.Attempt.String(), NativeRef: correlation.NativeRef, Kind: string(recorded.Kind),
			Action: correlation.ActionClass, ExpiresAt: request.ExpiresAt, Operation: request.Operation.String(), GrantID: view.GrantID, Scope: request.Scope})
		if err != nil {
			return nil, err
		}
		if _, err := u.uow.Exec(ctx, `update outbox set payload = $3::jsonb where tenant_id = $1 and outbox_id = $2`, tenant.String(), recorded.ID.String(), string(body)); err != nil {
			return nil, err
		}
	}
	return json.Marshal(view)
}
