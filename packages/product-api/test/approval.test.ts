import { describe, expect, it } from "vitest";
import { actionHash, approvalCoversExecution, canonicalJson, checkDecision, type ApprovalBinding } from "../src/index.js";

const intent = {
  operation: "repository.create_draft_pr",
  target: { connection_id: "conn_example", repo: "org/repo" },
  inputs: { base: "main", head_revision: "abc123" },
};

describe("canonical action hash (spec §13, §33)", () => {
  it("is key-order independent", () => {
    expect(canonicalJson({ b: 1, a: [1, { d: 2, c: 3 }] })).toBe('{"a":[1,{"c":3,"d":2}],"b":1}');
    const reordered = { inputs: { head_revision: "abc123", base: "main" }, target: { repo: "org/repo", connection_id: "conn_example" }, operation: intent.operation };
    expect(actionHash(reordered)).toBe(actionHash(intent));
    expect(actionHash(intent)).toMatch(/^sha256:[0-9a-f]{64}$/);
  });
  it("changes when head revision changes (new approval needed)", () => {
    expect(actionHash({ ...intent, inputs: { ...intent.inputs, head_revision: "def456" } })).not.toBe(actionHash(intent));
  });
  it("executor rejects changed input, policy or stale lease", () => {
    const approved = { action_hash: actionHash(intent), policy_version: "policy@5", lease_epoch: 3 };
    expect(approvalCoversExecution(approved, { intent, policy_version: "policy@5", lease_epoch: 3 })).toBe(true);
    expect(approvalCoversExecution(approved, { intent: { ...intent, target: { ...intent.target, repo: "org/other" } }, policy_version: "policy@5", lease_epoch: 3 })).toBe(false);
    expect(approvalCoversExecution(approved, { intent, policy_version: "policy@6", lease_epoch: 3 })).toBe(false);
    expect(approvalCoversExecution(approved, { intent, policy_version: "policy@5", lease_epoch: 4 })).toBe(false);
  });
});

describe("decision validation", () => {
  const b: ApprovalBinding = {
    tenant_id: "ten_a", task_id: "task_x", attempt_id: "att_1", current_attempt_id: "att_1", request_id: "req_1",
    request_version: 2, native_request_ref: "n", action_hash: actionHash(intent), policy_version: "policy@5",
    expires_at: "2026-10-12T22:00:00Z", authorized_responders: ["prn_bryan"], status: "pending",
  };
  const now = new Date("2026-10-12T21:00:00Z");
  const ok = { principal_id: "prn_bryan", tenant_id: "ten_a", request_version: 2, decision: "approve_once" as const };
  it("accepts a valid decision", () => expect(checkDecision(b, ok, now)).toEqual({ ok: true }));
  it("hides cross-tenant requests as not_found", () => expect(checkDecision(b, { ...ok, tenant_id: "ten_b" }, now)).toMatchObject({ error_code: "not_found" }));
  it("rejects unauthorized responder", () => expect(checkDecision(b, { ...ok, principal_id: "prn_other" }, now)).toMatchObject({ error_code: "forbidden" }));
  it("rejects stale screen version", () => expect(checkDecision(b, { ...ok, request_version: 1 }, now)).toMatchObject({ error_code: "version_conflict" }));
  it("rejects after expiry", () => expect(checkDecision(b, ok, new Date("2026-10-12T22:00:01Z"))).toMatchObject({ error_code: "expired" }));
  it("rejects requests from a superseded attempt", () => expect(checkDecision({ ...b, current_attempt_id: "att_2" }, ok, now)).toMatchObject({ error_code: "stale_attempt" }));
  it("rejects when not pending", () => expect(checkDecision({ ...b, status: "answered" }, ok, now)).toMatchObject({ error_code: "not_pending" }));
});
