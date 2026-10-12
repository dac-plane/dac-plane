/** Event envelope v1 and command envelope (spec §14). JSON Schemas in ../schemas. */
export const EVENT_SCHEMA_VERSION = "1.0";
export const MAX_INLINE_PAYLOAD_BYTES = 256 * 1024; // proposed default (§14)

export const EVENT_TYPES = [
  // task / execution
  "task.submitted", "attempt.allocated", "environment.ready", "runtime.started", "attempt.heartbeat",
  "attempt.lost", "task.cancel_requested",
  // runtime progress
  "turn.started", "message.delta", "tool.proposed", "tool.finished", "turn.finished", "checkpoint.created",
  // attention / policy
  "attention.requested", "decision.recorded", "decision.delivered", "decision.consumed", "action.denied",
  // results / accounting
  "artifact.created", "verification.finished", "result.accepted", "usage.recorded", "budget.threshold",
  "cleanup.finished",
] as const;
export type EventType = (typeof EVENT_TYPES)[number];

/** Event types that carry permission or state-changing semantics; unknown ones must be rejected. */
export const STATE_CHANGING_PREFIXES = ["task.", "attempt.", "attention.", "decision.", "action.", "result.", "environment."];

export interface EventEnvelope {
  schema_version: string; event_id: string; tenant_id: string; task_id: string; attempt_id: string | null;
  sequence: number; type: string; occurred_at: string; received_at: string; producer: string;
  correlation_id: string; causation_id: string | null; payload_ref: string | null; classification: string;
}

export const COMMAND_TYPES = ["StartAttempt", "SubmitTurn", "DeliverDecision", "CancelAttempt", "VerifyResult", "CleanupEnvironment"] as const;
export type CommandType = (typeof COMMAND_TYPES)[number];

export interface CommandEnvelope {
  command_id: string; type: CommandType; tenant_id: string; target_id: string; expected_version: number | null;
  lease_epoch: number | null; deadline: string; idempotency_key: string; payload_ref: string | null;
}

export type IngestDecision = "apply" | "store_generic" | "reject";

/**
 * §14 compatibility: unknown optional progress events are stored and shown generically; unknown
 * permission or state-changing semantics are rejected. An observation cannot grant permission by name.
 */
export function classifyEventType(type: string): IngestDecision {
  if ((EVENT_TYPES as readonly string[]).includes(type)) return "apply";
  if (STATE_CHANGING_PREFIXES.some((p) => type.startsWith(p)) || /approv|grant|permission/i.test(type)) return "reject";
  return "store_generic";
}
