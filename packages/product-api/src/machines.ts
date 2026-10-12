import { defineMachine } from "./state-machine.js";

/* ---------- Task (spec §11) ---------- */
export const TASK_STATES = [
  "draft", "queued", "running", "waiting_attention", "verifying", "ready_for_review",
  "cancelling", "succeeded", "failed", "cancelled",
] as const;
export type TaskState = (typeof TASK_STATES)[number];
export type TaskEvent =
  | "submit" | "dispatch" | "cancel" | "wait" | "output" | "error"
  | "response_consumed" | "attention_denied" | "attention_unresolved" | "verification_passed" | "verification_mismatch"
  | "accept" | "request_changes" | "stop_reconciled" | "stop_uncertain";

export const taskMachine = defineMachine<TaskState, TaskEvent>(
  "task",
  {
    draft: { submit: ["queued"] },
    queued: { dispatch: ["running"], cancel: ["cancelled"] },
    // ADR-0011(b): during active execution cancel records `cancelling` until stop is confirmed
    running: { wait: ["waiting_attention"], output: ["verifying"], error: ["failed"], cancel: ["cancelling"] },
    waiting_attention: {
      response_consumed: ["running"],
      // ADR-0011(b): deny closes that request as denied and blocks that action only; work continues.
      attention_denied: ["running"],
      // timeout -> running with restriction, failed or cancelling according to request policy
      attention_unresolved: ["running", "failed", "cancelling"],
      // Cancel allowed while waiting; the native wait/process may be live, so stop must be confirmed.
      cancel: ["cancelling"],
    },
    verifying: {
      // pass -> ready_for_review, or succeeded only under explicit auto-accept policy (see guard below)
      verification_passed: ["ready_for_review", "succeeded"],
      verification_mismatch: ["running", "failed"],
      cancel: ["cancelling"],
    },
    ready_for_review: { accept: ["succeeded"], request_changes: ["queued"], cancel: ["cancelled"] },
    // uncertainty -> remains cancelling with escalation. No verification/output events are accepted
    // here: a late verification result can never move a cancelling/cancelled task to success.
    cancelling: { stop_reconciled: ["cancelled"], stop_uncertain: ["cancelling"] },
    succeeded: {}, failed: {}, cancelled: {},
  },
  ["succeeded", "failed", "cancelled"],
);

/** §21: automatic acceptance only under an explicit task policy with objective checks. */
export function verificationPassedTarget(autoAcceptPolicy: boolean, objectiveChecksOnly: boolean): TaskState {
  return autoAcceptPolicy && objectiveChecksOnly ? "succeeded" : "ready_for_review";
}

/**
 * §11: a native end-of-turn moves work to verification only when the adapter reports a proposed
 * result. Idle, token limit and user-cancel stop reasons are not successful completion.
 */
export type StopReason = "proposed_result" | "idle" | "token_limit" | "user_cancel" | "error" | "unknown";
export function taskEventForStopReason(reason: StopReason): TaskEvent | null {
  switch (reason) {
    case "proposed_result": return "output";
    case "error": return "error";
    case "user_cancel": return "cancel";
    default: return null; // stays running; needs follow-up or user decision
  }
}

/**
 * Late observations (e.g. a verification result arriving after cancel) are retained for audit but
 * never applied. Returns the event to apply, or "audit_only".
 */
export function routeTaskObservation(state: TaskState, event: TaskEvent): TaskEvent | "audit_only" {
  return taskMachine.targets(state, event).length > 0 ? event : "audit_only";
}

/* ---------- Attempt (spec §11) ---------- */
export const ATTEMPT_STATES = [
  "allocated", "provisioning", "starting", "executing", "waiting", "finalizing", "completed",
  "failed", "lost", "cancellation_requested", "terminated",
] as const;
export type AttemptState = (typeof ATTEMPT_STATES)[number];
export type AttemptEvent =
  | "provision" | "start" | "runtime_started" | "wait" | "resume" | "finalize" | "complete"
  | "fail" | "lease_expired" | "request_cancel" | "terminated_confirmed" | "reconcile";

const cancellable = { request_cancel: ["cancellation_requested"], fail: ["failed"], lease_expired: ["lost"] } as const;
export const attemptMachine = defineMachine<AttemptState, AttemptEvent>(
  "attempt",
  {
    allocated: { provision: ["provisioning"], ...cancellable },
    provisioning: { start: ["starting"], ...cancellable },
    starting: { runtime_started: ["executing"], ...cancellable },
    executing: { wait: ["waiting"], finalize: ["finalizing"], ...cancellable },
    waiting: { resume: ["executing"], ...cancellable },
    finalizing: { complete: ["completed"], ...cancellable },
    // a lost attempt does not automatically mean a failed task; reconciliation decides
    lost: { reconcile: ["terminated", "failed"], request_cancel: ["cancellation_requested"] },
    cancellation_requested: { terminated_confirmed: ["terminated"], lease_expired: ["lost"] },
    completed: {}, failed: {}, terminated: {},
  },
  ["completed", "failed", "terminated"],
);

/* ---------- Attention request (spec §13) ---------- */
export const ATTENTION_STATES = [
  "created", "pending", "answered", "dispatching", "delivered", "consumed", "closed",
  "denied", "expired", "cancelled", "superseded",
] as const;
export type AttentionState = (typeof ATTENTION_STATES)[number];
export type AttentionEvent =
  | "publish" | "answer" | "deny" | "dispatch" | "redeliver" | "delivered" | "consumed" | "close"
  | "expire" | "cancel" | "supersede";

export const attentionMachine = defineMachine<AttentionState, AttentionEvent>(
  "attention_request",
  {
    created: { publish: ["pending"], expire: ["expired"], cancel: ["cancelled"], supersede: ["superseded"] },
    pending: {
      answer: ["answered"], deny: ["denied"], expire: ["expired"], cancel: ["cancelled"], supersede: ["superseded"],
    },
    answered: { dispatch: ["dispatching"], cancel: ["cancelled"], supersede: ["superseded"] },
    // retry delivery with the same decision ID
    dispatching: { redeliver: ["dispatching"], delivered: ["delivered"], cancel: ["cancelled"], supersede: ["superseded"] },
    delivered: { consumed: ["consumed"], cancel: ["cancelled"], supersede: ["superseded"] },
    consumed: { close: ["closed"] },
    closed: {}, denied: {}, expired: {}, cancelled: {}, superseded: {},
  },
  ["closed", "denied", "expired", "cancelled", "superseded"],
);

/* ---------- Environment (spec §11) ---------- */
export const ENVIRONMENT_STATES = [
  "requested", "provisioning", "ready", "active", "draining", "stopped", "destroyed", "failed", "quarantined",
] as const;
export type EnvironmentState = (typeof ENVIRONMENT_STATES)[number];
export type EnvironmentEvent = "provision" | "ready" | "activate" | "drain" | "stop" | "destroy" | "fail" | "quarantine";

export const environmentMachine = defineMachine<EnvironmentState, EnvironmentEvent>(
  "environment",
  {
    requested: { provision: ["provisioning"], fail: ["failed"] },
    provisioning: { ready: ["ready"], fail: ["failed"], quarantine: ["quarantined"] },
    ready: { activate: ["active"], drain: ["draining"], fail: ["failed"], quarantine: ["quarantined"] },
    active: { drain: ["draining"], fail: ["failed"], quarantine: ["quarantined"] },
    draining: { stop: ["stopped"], fail: ["failed"], quarantine: ["quarantined"] },
    stopped: { destroy: ["destroyed"] },
    failed: { quarantine: ["quarantined"], stop: ["stopped"], destroy: ["destroyed"] },
    quarantined: { stop: ["stopped"], destroy: ["destroyed"] },
    destroyed: {},
  },
  ["destroyed"],
);

/* ---------- Connection (spec §19) ---------- */
export const CONNECTION_STATES = [
  "requested", "authorizing", "active", "refresh_needed", "revoked", "expired", "error",
] as const;
export type ConnectionState = (typeof CONNECTION_STATES)[number];
export type ConnectionEvent = "authorize" | "authorized" | "refresh_due" | "refreshed" | "revoke" | "expire" | "fail" | "reauthorize";

export const connectionMachine = defineMachine<ConnectionState, ConnectionEvent>(
  "connection",
  {
    requested: { authorize: ["authorizing"], revoke: ["revoked"] },
    authorizing: { authorized: ["active"], fail: ["error"], revoke: ["revoked"] },
    active: { refresh_due: ["refresh_needed"], revoke: ["revoked"], expire: ["expired"], fail: ["error"] },
    refresh_needed: { refreshed: ["active"], revoke: ["revoked"], expire: ["expired"], fail: ["error"] },
    expired: { reauthorize: ["authorizing"], revoke: ["revoked"] },
    error: { reauthorize: ["authorizing"], revoke: ["revoked"] },
    revoked: {},
  },
  ["revoked"],
);

/** Blocked connection states (§19): calls through them must fail. */
export const CONNECTION_BLOCKED: readonly ConnectionState[] = ["revoked", "expired", "error", "requested", "authorizing", "refresh_needed"];
