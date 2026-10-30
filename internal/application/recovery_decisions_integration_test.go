//go:build integration

package application_test

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
)

func TestRecoveryDecisionErrorsRespectFences(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"transport", "unknown", "malformed"} {
		for _, fence := range []string{"outbox_expiry", "attempt_expiry", "outbox_takeover", "attempt_takeover"} {
			t.Run(outcome+"/"+fence, func(t *testing.T) {
				f := newFixture(t, lifecycle.HarnessLive)
				s, rt := f.executing(t, "decision-error")
				f.raise(t, rt, s, 2, question("question", f.at.Add(time.Hour)))
				cmd := f.decideCommand("answer", f.attention(t, s), lifecycle.DecisionDeny)
				cmd.At = f.at
				view := f.decide(t, cmd)
				steer := f.steer(t, s)
				var before map[string]string
				p := &recoveryPort{Port: steer, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
					reply, err := steer.Send(ctx, r)
					if err != nil {
						t.Fatal(err)
					}
					switch fence {
					case "outbox_expiry":
						_, err = f.pool.Exec(ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
					case "attempt_expiry":
						_, err = f.pool.Exec(ctx, `update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
					case "outbox_takeover":
						_, err = f.pool.Exec(ctx, `update outbox set lease_epoch=lease_epoch+1,claimed_by='replacement' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
					case "attempt_takeover":
						_, err = f.pool.Exec(ctx, `update attempt set lease_epoch=lease_epoch+1,lease_owner='replacement' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
					}
					if err != nil {
						t.Fatal(err)
					}
					before = f.recoverySnapshot(t)
					switch outcome {
					case "transport":
						return contracts.Reply{}, io.ErrUnexpectedEOF
					case "unknown":
						reply.Durability = lifecycle.SendUnknown
					case "malformed":
						reply.Outcome = "invalid"
					}
					return reply, nil
				}}
				if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
				if before == nil || !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
					t.Fatal("stale error handler mutated authoritative records")
				}
				if steer.Ledger().Effects(s.Operation.ID) != 1 {
					t.Fatal("decision effect count changed")
				}
			})
		}
	}
}

func TestRecoveryDecisionQualificationBeforeSend(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"shadow", "off"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, lifecycle.HarnessLive)
			s, rt := f.executing(t, "decision-qualification")
			f.raise(t, rt, s, 2, question("question", f.at.Add(time.Hour)))
			cmd := f.decideCommand("answer", f.attention(t, s), lifecycle.DecisionDeny)
			cmd.At = f.at
			f.decide(t, cmd)
			if _, err := f.pool.Exec(f.ctx, `update harness_qualification set mode=$2,version=version+1 where tenant_id=$1`, f.tenant.String(), mode); err != nil {
				t.Fatal(err)
			}
			before := f.recoverySnapshot(t)
			steer := f.steer(t, s)
			f.reopen(t)
			if err := recoveryWorker(t, f, steer).Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			after := f.recoverySnapshot(t)
			// Yielding the queue claim is allowed; primary lifecycle is not.
			delete(before, "outbox")
			delete(after, "outbox")
			if !reflect.DeepEqual(before, after) || len(steer.Ledger().Sent()) != 0 {
				t.Fatal("non-live decision mutated primary state or called the runtime")
			}
			if _, err := f.pool.Exec(f.ctx, `update harness_qualification set mode='live',version=version+1 where tenant_id=$1`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := recoveryWorker(t, f, steer).Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			if steer.Ledger().Effects(s.Operation.ID) != 1 {
				t.Fatal("qualification gate lost the queued decision")
			}
		})
	}
}

func TestRecoveryDecisionDemotionBeforeSettlement(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"accepted", "rejected", "transport", "malformed"} {
		for _, mode := range []string{"shadow", "off"} {
			t.Run(outcome+"/"+mode, func(t *testing.T) {
				f := newFixture(t, lifecycle.HarnessLive)
				s, rt := f.executing(t, "decision-demotion")
				f.raise(t, rt, s, 2, question("question", f.at.Add(time.Hour)))
				cmd := f.decideCommand("answer", f.attention(t, s), lifecycle.DecisionDeny)
				cmd.At = f.at
				f.decide(t, cmd)
				steer := f.steer(t, s)
				var before map[string]string
				p := &recoveryPort{Port: steer, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
					reply, err := steer.Send(ctx, r)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.pool.Exec(ctx, `update harness_qualification set mode=$2,version=version+1 where tenant_id=$1`, f.tenant.String(), mode); err != nil {
						t.Fatal(err)
					}
					before = f.recoverySnapshot(t)
					switch outcome {
					case "rejected":
						reply.Durability = lifecycle.SendRejected
						reply.Outcome = contracts.OutcomeFailed
						reply.Failure = &contracts.Failure{Code: contracts.FailureProviderFailed, Message: "rejected"}
					case "transport":
						return contracts.Reply{}, io.ErrUnexpectedEOF
					case "malformed":
						reply.Outcome = "invalid"
					}
					return reply, nil
				}}
				if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
				if before == nil || !reflect.DeepEqual(before, f.recoverySnapshot(t)) {
					t.Fatal("demoted decision handler committed primary changes")
				}
				if steer.Ledger().Effects(s.Operation.ID) != 1 {
					t.Fatal("decision effect count changed")
				}
			})
		}
	}
}

func TestRecoveryLostDecisionFenceKeepsBatchMoving(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"accepted", "transport", "malformed"} {
		for _, fence := range []string{"outbox_expiry", "attempt_expiry", "outbox_takeover", "attempt_takeover", "late_attempt_expiry"} {
			if fence == "late_attempt_expiry" && outcome != "accepted" {
				continue
			}
			t.Run(outcome+"/"+fence, func(t *testing.T) {
				f := newFixture(t, lifecycle.HarnessLive)
				s, rt := f.executing(t, "batch-decision")
				f.raise(t, rt, s, 2, question("question", f.at.Add(time.Hour)))
				cmd := f.decideCommand("answer", f.attention(t, s), lifecycle.DecisionDeny)
				cmd.At = f.at
				view := f.decide(t, cmd)
				nextCommand := f.submitCommand("next-in-batch")
				nextCommand.Demand = contracts.DemandBackground
				next := f.submit(t, nextCommand)
				steer := f.steer(t, s)
				if fence == "late_attempt_expiry" {
					// Expire after the initial fence check, inside the same
					// settlement transaction. Its final check must roll it all back.
					if _, err := f.pool.Exec(f.ctx, `create function recovery_expire_decision_lease() returns trigger language plpgsql as $$
						begin update attempt set lease_expires_at=clock_timestamp()-interval '1 second'
						where tenant_id=NEW.tenant_id and attempt_id=NEW.attempt_id; return NEW; end $$;
						create trigger recovery_expire_decision_lease after update on attention_request
						for each row when (NEW.state='delivered') execute function recovery_expire_decision_lease()`); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := func() string {
					var raw string
					if err := f.pool.QueryRow(f.ctx, `select jsonb_build_object(
						'outbox',(select to_jsonb(o) from outbox o where tenant_id=$1 and outbox_id=$2),
						'attempt',(select to_jsonb(a) from attempt a where tenant_id=$1 and attempt_id=$3))::text`,
						f.tenant.String(), view.DecisionID, s.Attempt.ID.String()).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					return raw
				}
				var before string
				nextEffects := 0
				p := &recoveryPort{Port: steer, send: func(ctx context.Context, r contracts.Request) (contracts.Reply, error) {
					if r.Operation == next.Operation.ID {
						if before == "" {
							t.Fatal("follow-up dispatched before the person-demand decision")
						}
						nextEffects++
						return contracts.Reply{Operation: r.Operation, Delivery: r.Delivery, Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted}, nil
					}
					reply, err := steer.Send(ctx, r)
					if err != nil {
						t.Fatal(err)
					}
					switch fence {
					case "outbox_expiry":
						_, err = f.pool.Exec(ctx, `update outbox set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
					case "attempt_expiry":
						_, err = f.pool.Exec(ctx, `update attempt set lease_expires_at=clock_timestamp()-interval '1 second' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
					case "outbox_takeover":
						_, err = f.pool.Exec(ctx, `update outbox set lease_epoch=lease_epoch+1,claimed_by='replacement' where tenant_id=$1 and outbox_id=$2`, f.tenant.String(), view.DecisionID)
					case "attempt_takeover":
						_, err = f.pool.Exec(ctx, `update attempt set lease_epoch=lease_epoch+1,lease_owner='replacement' where tenant_id=$1 and attempt_id=$2`, f.tenant.String(), s.Attempt.ID.String())
					}
					if err != nil {
						t.Fatal(err)
					}
					before = snapshot()
					if outcome == "transport" {
						return contracts.Reply{}, io.ErrUnexpectedEOF
					}
					if outcome == "malformed" {
						reply.Outcome = "invalid"
					}
					return reply, nil
				}}
				if err := recoveryWorker(t, f, p).Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
				op, err := store.GetOperation(f.ctx, f.pool, f.tenant, next.Operation.ID)
				if err != nil || op.State != lifecycle.OperationAccepted || nextEffects != 1 {
					t.Fatalf("following row state=%s effects=%d err=%v", op.State, nextEffects, err)
				}
				if before == "" || snapshot() != before || steer.Ledger().Effects(s.Operation.ID) != 1 {
					t.Fatal("stale settlement changed its records or repeated its effect")
				}
			})
		}
	}
}
