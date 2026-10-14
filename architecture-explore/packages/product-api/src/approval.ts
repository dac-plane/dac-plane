import { actionHash, type ActionIntent } from "./action-hash.js";
import type { AttentionState } from "./machines.js";

/** Approval binding fields (spec §13, §33). */
export interface ApprovalBinding {
  tenant_id: string;
  task_id: string;
  attempt_id: string;
  request_id: string;
  request_version: number;
  native_request_ref: string;
  action_hash: string;
  policy_version: string;
  expires_at: string;
  authorized_responders: readonly string[];
  status: AttentionState;
  current_attempt_id: string;
}

export interface DecisionSubmission {
  /** Derived from authentication, never from the request body. */
  principal_id: string;
  tenant_id: string;
  request_version: number;
  decision: "approve_once" | "deny";
}

export type DecisionCheck =
  | { ok: true }
  | { ok: false; error_code: "not_found" | "forbidden" | "version_conflict" | "expired" | "not_pending" | "stale_attempt" };

/** Validate a responder's decision before the atomic compare-and-set (first valid decision wins). */
export function checkDecision(b: ApprovalBinding, d: DecisionSubmission, now: Date): DecisionCheck {
  // Do not leak other tenants' resource existence (§15): cross-tenant looks like not found.
  if (b.tenant_id !== d.tenant_id) return { ok: false, error_code: "not_found" };
  if (!b.authorized_responders.includes(d.principal_id)) return { ok: false, error_code: "forbidden" };
  if (b.status !== "pending") return { ok: false, error_code: "not_pending" };
  if (b.attempt_id !== b.current_attempt_id) return { ok: false, error_code: "stale_attempt" };
  if (Date.parse(b.expires_at) <= now.getTime()) return { ok: false, error_code: "expired" };
  if (b.request_version !== d.request_version) return { ok: false, error_code: "version_conflict" };
  return { ok: true };
}

/**
 * Executor-side revalidation immediately before execution (§13, §33): the action about to run must
 * hash to the approved action, under the same policy version, by the current lease owner.
 */
export function approvalCoversExecution(
  approved: { action_hash: string; policy_version: string; lease_epoch: number },
  executing: { intent: ActionIntent; policy_version: string; lease_epoch: number },
): boolean {
  return (
    approved.action_hash === actionHash(executing.intent) &&
    approved.policy_version === executing.policy_version &&
    approved.lease_epoch === executing.lease_epoch
  );
}
