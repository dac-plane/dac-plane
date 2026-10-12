import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import { parse } from "yaml";
import {
  classifyEventType, deriveTier, manifestProblems, supportsActionApprovals, EVENT_TYPES, TASK_STATES,
  type CapabilityManifest,
} from "../src/index.js";

const root = join(__dirname, "..");
const ajv = new (Ajv2020 as any)({ allErrors: true, strict: false });
(addFormats as any)(ajv);
const schemas: Record<string, any> = {};
for (const f of readdirSync(join(root, "schemas"))) {
  const s = JSON.parse(readFileSync(join(root, "schemas", f), "utf8"));
  ajv.addSchema(s, f);
  schemas[f] = s;
}
const v = (name: string, data: unknown) => ajv.validate(name, data) as boolean;

describe("JSON Schemas (spec §14, §33)", () => {
  it("all schemas compile", () => {
    for (const f of Object.keys(schemas)) expect(() => ajv.getSchema(f)).not.toThrow();
  });
  it("accepts the worked task submission (§32)", () => {
    expect(v("task-submission.schema.json", {
      project_id: "proj_example", profile_version: "engineering@3",
      objective: "Fix the login error on an expired session",
      acceptance_criteria: ["Expired session redirects to sign-in", "Regression test reproduces and verifies the fix", "Existing targeted test suite passes"],
      runtime_constraints: { permission_callbacks: true }, budget: { max_duration_seconds: 1800 }, external_effects: "no_publish",
    })).toBe(true);
  });
  it("rejects a task without acceptance criteria", () => {
    expect(v("task-submission.schema.json", { project_id: "p", profile_version: "e@1", objective: "x", acceptance_criteria: [] })).toBe(false);
  });
  const env = {
    schema_version: "1.0", event_id: "evt_0123456789", tenant_id: "ten_0123456789", task_id: "task_0123456789",
    attempt_id: "att_0123456789", sequence: 17, type: "attention.requested", occurred_at: "2026-10-12T21:00:00Z",
    received_at: "2026-10-12T21:00:01Z", producer: "adapter/sim@0.1", correlation_id: "corr_1", causation_id: "evt_x",
    payload_ref: "obj://x", classification: "project_internal",
  };
  it("accepts a valid event envelope and allows additive fields", () => {
    expect(v("event-envelope.schema.json", env)).toBe(true);
    expect(v("event-envelope.schema.json", { ...env, new_optional_field: 1 })).toBe(true);
  });
  it("rejects major-version or malformed envelopes", () => {
    expect(v("event-envelope.schema.json", { ...env, schema_version: "2.0" })).toBe(false);
    expect(v("event-envelope.schema.json", { ...env, sequence: 0 })).toBe(false);
  });
  it("action approval requires binding fields", () => {
    const base = { request_id: "req_1", request_version: 2, type: "action_approval", task_id: "t", attempt_id: "a",
      native_request_ref: "n", policy_version: "policy@5", expires_at: "2026-10-12T22:00:00Z", authorized_responders: ["p"] };
    expect(v("attention-request.schema.json", base)).toBe(false);
    expect(v("attention-request.schema.json", { ...base, operation: "repository.create_draft_pr", target: {}, inputs: {},
      action_hash: "sha256:" + "a".repeat(64) })).toBe(true);
  });
  it("attention response never carries a principal", () => {
    expect(v("attention-response.schema.json", { request_version: 2, decision: "approve_once" })).toBe(true);
    expect(v("attention-response.schema.json", { request_version: 2, decision: "approve_once", principal_id: "x" })).toBe(false);
    expect(v("attention-response.schema.json", { request_version: 2, decision: "answer" })).toBe(false);
  });
});

describe("event ingestion compatibility (§14)", () => {
  it("known types apply; unknown progress is generic; unknown permission/state semantics rejected", () => {
    for (const t of EVENT_TYPES) expect(classifyEventType(t)).toBe("apply");
    expect(classifyEventType("progress.note")).toBe("store_generic");
    expect(classifyEventType("action.approved")).toBe("reject");
    expect(classifyEventType("task.force_succeed")).toBe("reject");
    expect(classifyEventType("custom.grant_added")).toBe("reject");
  });
});

describe("capability manifest (§16, §33)", () => {
  const caps = { structured_events: true, permission_callbacks: true, follow_up: "queued", cancel: "native_then_process_stop",
    recovery: "restart_from_artifacts", usage: "partial", concurrent_turns: false, secure_auth_handoff: false } as const;
  const m: CapabilityManifest = { adapter: "example@1.0", runtime: "pinned-version", protocol: "negotiated-and-pinned", tier: "B", capabilities: caps, conformance_report_ref: "r" };
  it("illustrative manifest from §33 is Tier B and schema-valid", () => {
    expect(deriveTier(caps)).toBe("B");
    expect(manifestProblems(m)).toEqual([]);
    expect(v("capability-manifest.schema.json", m)).toBe(true);
  });
  it("over-declared tier and missing conformance report are flagged", () => {
    expect(manifestProblems({ ...m, tier: "A", conformance_report_ref: null })).toHaveLength(2);
  });
  it("Tier C adapters never advertise action approvals", () => {
    const c = { ...m, tier: "C" as const, capabilities: { ...caps, structured_events: false } };
    expect(supportsActionApprovals(c)).toBe(false);
    expect(manifestProblems(c)).toContain("Tier C adapter must not advertise action-level approvals");
  });
});

describe("OpenAPI (§15)", () => {
  const doc = parse(readFileSync(join(root, "openapi.yaml"), "utf8"));
  it("declares every endpoint from the spec", () => {
    const expected: [string, string][] = [
      ["/v1/tasks", "post"], ["/v1/tasks/{id}", "get"], ["/v1/tasks/{id}/events", "get"], ["/v1/tasks/{id}/messages", "post"],
      ["/v1/tasks/{id}/cancel", "post"], ["/v1/tasks/{id}/retry", "post"], ["/v1/attention", "get"],
      ["/v1/attention/{id}/responses", "post"], ["/v1/tasks/{id}/reviews", "post"], ["/v1/artifacts/{id}", "get"],
      ["/v1/connections", "post"], ["/v1/connections/{id}/revoke", "post"], ["/v1/capabilities", "get"],
    ];
    for (const [p, m] of expected) expect(doc.paths[p]?.[m], `${m} ${p}`).toBeDefined();
  });
  it("task creation requires Idempotency-Key", () => {
    expect(doc.components.parameters.IdempotencyKey.required).toBe(true);
  });
  it("task snapshot states match the domain state machine", () => {
    expect(doc.components.schemas.TaskSnapshot.properties.state.enum).toEqual([...TASK_STATES]);
  });
  it("all local $refs resolve to schema files", () => {
    const refs = JSON.stringify(doc).match(/"\.\/schemas\/[^"]+"/g) ?? [];
    for (const r of refs) expect(schemas[r.slice(11, -1)], r).toBeDefined();
  });
});
