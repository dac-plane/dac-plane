import { describe, expect, it } from "vitest";
import {
  attemptMachine, attentionMachine, connectionMachine, environmentMachine, taskMachine,
  taskEventForStopReason, verificationPassedTarget, routeTaskObservation, TransitionError,
  type StateMachine,
} from "../src/index.js";

function reachable<S extends string, E extends string>(m: StateMachine<S, E>, start: S): Set<S> {
  const seen = new Set<S>([start]);
  const q = [start];
  while (q.length) {
    const s = q.shift()!;
    for (const e of allEvents(m)) for (const t of m.targets(s, e)) if (!seen.has(t)) { seen.add(t); q.push(t); }
  }
  return seen;
}
function allEvents<S extends string, E extends string>(m: StateMachine<S, E>): E[] {
  const out = new Set<E>();
  const probe = ["submit","dispatch","cancel","wait","output","error","stop","response_consumed","attention_unresolved",
    "verification_passed","verification_mismatch","attention_denied","accept","request_changes","stop_reconciled","stop_uncertain",
    "provision","start","runtime_started","resume","finalize","complete","fail","lease_expired","request_cancel",
    "terminated_confirmed","reconcile","publish","answer","deny","redeliver","delivered","consumed","close","expire",
    "supersede","ready","activate","drain","destroy","quarantine","authorize","authorized","refresh_due","refreshed",
    "revoke","reauthorize"] as E[];
  for (const s of m.states) for (const e of probe) if (m.targets(s, e).length) out.add(e);
  return [...out];
}

const machines = [
  [taskMachine, "draft"], [attemptMachine, "allocated"], [attentionMachine, "created"],
  [environmentMachine, "requested"], [connectionMachine, "requested"],
] as const;

describe.each(machines)("%s structural invariants", (m: any, start: string) => {
  it("terminal states have no outgoing transitions", () => {
    for (const t of m.terminal) for (const e of allEvents(m)) expect(m.targets(t, e)).toEqual([]);
  });
  it("all states reachable from the initial state", () => {
    expect([...reachable(m, start)].sort()).toEqual([...m.states].sort());
  });
  it("every non-terminal state can reach a terminal state", () => {
    for (const s of m.states) {
      const r = reachable(m, s);
      expect(m.terminal.some((t: string) => r.has(t)), `${m.name}:${s}`).toBe(true);
    }
  });
  it("all transition targets are known states", () => {
    for (const s of m.states) for (const e of allEvents(m)) for (const t of m.targets(s, e)) expect(m.states).toContain(t);
  });
});

describe("task machine (spec §11)", () => {
  const t0 = { state: "draft" as const, version: 0 };

  it("happy path draft -> succeeded with review", () => {
    let t: { state: any; version: number } = t0;
    for (const [ev, to] of [["submit"], ["dispatch"], ["wait"], ["response_consumed"], ["output"],
      ["verification_passed", "ready_for_review"], ["accept"]] as const) {
      t = taskMachine.apply(t, ev as any, t.version, to as any);
    }
    expect(t).toEqual({ state: "succeeded", version: 7 });
  });

  it("rejects stale expected version (409 semantics)", () => {
    const t = taskMachine.apply(t0, "submit", 0);
    expect(() => taskMachine.apply(t, "dispatch", 0)).toThrowError(TransitionError);
    try { taskMachine.apply(t, "dispatch", 0); } catch (e) { expect((e as TransitionError).code).toBe("version_conflict"); }
  });

  it("rejects transitions out of terminal states", () => {
    expect(() => taskMachine.apply({ state: "cancelled", version: 3 }, "dispatch", 3)).toThrow(/terminal/);
  });

  it("cannot cancel a running task directly; it goes through cancelling", () => {
    expect(taskMachine.targets("running", "cancel")).toEqual(["cancelling"]);
    expect(taskMachine.apply({ state: "running", version: 1 }, "cancel", 1).state).toBe("cancelling");
  });

  it("cancelling remains until stop is reconciled; uncertainty keeps it cancelling", () => {
    const c = taskMachine.apply({ state: "cancelling", version: 1 }, "stop_uncertain", 1);
    expect(c.state).toBe("cancelling");
    expect(taskMachine.apply(c, "stop_reconciled", c.version).state).toBe("cancelled");
  });

  it("verification pass with multiple targets requires an explicit target", () => {
    expect(() => taskMachine.apply({ state: "verifying", version: 1 }, "verification_passed", 1)).toThrow(/explicit target/);
  });

  it("auto-accept only with explicit policy and objective checks", () => {
    expect(verificationPassedTarget(false, true)).toBe("ready_for_review");
    expect(verificationPassedTarget(true, false)).toBe("ready_for_review");
    expect(verificationPassedTarget(true, true)).toBe("succeeded");
  });

  it("request_changes creates queued new-attempt state", () => {
    expect(taskMachine.apply({ state: "ready_for_review", version: 5 }, "request_changes", 5).state).toBe("queued");
  });

  it("idle/token limit stop reasons are not task completion", () => {
    expect(taskEventForStopReason("idle")).toBeNull();
    expect(taskEventForStopReason("token_limit")).toBeNull();
    expect(taskEventForStopReason("user_cancel")).toBe("cancel");
    expect(taskEventForStopReason("proposed_result")).toBe("output");
  });

  it("succeeded is only reachable via review accept or verification pass", () => {
    for (const s of taskMachine.states) for (const e of allEvents(taskMachine)) {
      if (taskMachine.targets(s, e).includes("succeeded")) expect(["accept", "verification_passed"]).toContain(e);
    }
  });
});

describe("ADR-0011(b) interpretations", () => {
  it("deny returns the task to running (blocks the action only, not the task)", () => {
    expect(taskMachine.apply({ state: "waiting_attention", version: 2 }, "attention_denied", 2).state).toBe("running");
    expect(attentionMachine.apply({ state: "pending", version: 1 }, "deny", 1).state).toBe("denied");
    expect(attentionMachine.isTerminal("denied")).toBe(true);
  });
  it("cancel is allowed while waiting and verifying and goes through cancelling", () => {
    expect(taskMachine.targets("waiting_attention", "cancel")).toEqual(["cancelling"]);
    expect(taskMachine.targets("verifying", "cancel")).toEqual(["cancelling"]);
  });
  it("late verification results never move cancelling/cancelled to success", () => {
    for (const s of ["cancelling", "cancelled"] as const) {
      for (const e of ["verification_passed", "output", "accept", "response_consumed"] as const) {
        expect(routeTaskObservation(s, e)).toBe("audit_only");
      }
    }
    expect(() => taskMachine.apply({ state: "cancelling", version: 4 }, "verification_passed", 4, "succeeded")).toThrow(TransitionError);
    expect(() => taskMachine.apply({ state: "cancelled", version: 5 }, "verification_passed", 5, "succeeded")).toThrow(/terminal/);
  });
});

describe("attempt machine (spec §11)", () => {
  it("normal lifecycle with a wait", () => {
    let a: { state: any; version: number } = { state: "allocated", version: 0 };
    for (const ev of ["provision", "start", "runtime_started", "wait", "resume", "finalize", "complete"]) {
      a = attemptMachine.apply(a, ev as any, a.version);
    }
    expect(a.state).toBe("completed");
  });
  it("lost is not terminal and must be reconciled", () => {
    expect(attemptMachine.isTerminal("lost")).toBe(false);
    expect(attemptMachine.targets("lost", "reconcile")).toEqual(["terminated", "failed"]);
  });
  it("cancellation_requested ends only on confirmed termination (or lease loss)", () => {
    expect(attemptMachine.targets("cancellation_requested", "complete")).toEqual([]);
    expect(attemptMachine.apply({ state: "cancellation_requested", version: 2 }, "terminated_confirmed", 2).state).toBe("terminated");
  });
});

describe("attention machine (spec §13)", () => {
  it("full delivery path; answer is not proof of resume", () => {
    let r: { state: any; version: number } = { state: "created", version: 0 };
    for (const ev of ["publish", "answer", "dispatch", "redeliver", "delivered", "consumed", "close"]) {
      r = attentionMachine.apply(r, ev as any, r.version);
    }
    expect(r.state).toBe("closed");
    expect(attentionMachine.isTerminal("answered")).toBe(false);
  });
  it("expired or superseded requests cannot be answered", () => {
    for (const s of ["expired", "superseded", "cancelled", "denied"] as const) {
      expect(attentionMachine.can(s, "answer")).toBe(false);
    }
  });
  it("cannot expire after an answer was recorded", () => {
    expect(attentionMachine.can("answered", "expire")).toBe(false);
  });
  it("a second answer is rejected (first valid decision wins)", () => {
    const r = attentionMachine.apply({ state: "pending", version: 1 }, "answer", 1);
    expect(() => attentionMachine.apply(r, "answer", r.version)).toThrow(TransitionError);
  });
});

describe("environment and connection machines", () => {
  it("environment: cleanup requires stopping before destroy", () => {
    expect(environmentMachine.can("active", "destroy")).toBe(false);
    expect(environmentMachine.can("stopped", "destroy")).toBe(true);
  });
  it("connection: revoked is final; expired can reauthorize", () => {
    expect(connectionMachine.isTerminal("revoked")).toBe(true);
    expect(connectionMachine.apply({ state: "expired", version: 0 }, "reauthorize", 0).state).toBe("authorizing");
  });
});
