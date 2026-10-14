/** Domain model (spec §9). Field names match the spec; storage in services/product-tasks/migrations. */
import type * as I from "./ids.js";
import type { AttemptState, AttentionState, ConnectionState, EnvironmentState, TaskState } from "./machines.js";

export type Timestamp = string; // RFC3339
export type Classification = "public" | "project_internal" | "confidential" | "restricted";

export interface Tenant { id: I.TenantId; policy_version: string }
export interface Principal { id: I.PrincipalId; memberships: { tenant_id: I.TenantId; roles: string[] }[] }

export interface Project {
  id: I.ProjectId; tenant_id: I.TenantId; repository_connection_id: I.ConnectionId | null;
  base_revision: string | null; storage_ref: string | null; trust_group: string;
}

export interface AgentProfileVersion {
  tenant_id: I.TenantId; profile_id: I.ProfileId; version: number; instructions_ref: string;
  runtime_constraints: Record<string, unknown>; tool_policy_ref: string; memory_scope: string;
}

export type ExternalEffects = "none" | "no_publish" | "draft_only" | "ask";

export interface Budget {
  max_duration_seconds?: number; max_cost_minor_units?: number; currency?: string;
}

export interface Task {
  id: I.TaskId; tenant_id: I.TenantId; project_id: I.ProjectId; owner_id: I.PrincipalId;
  profile_version: string; objective: string; acceptance_criteria: string[]; state: TaskState;
  version: number; budget_ref: string | null; external_effects: ExternalEffects;
  auto_accept: boolean; active_attempt_id: I.AttemptId | null;
  created_at: Timestamp; updated_at: Timestamp;
}

export type RecoveryMode = "exact_resume" | "restart_from_artifacts" | "unsupported";

export interface Attempt {
  id: I.AttemptId; tenant_id: I.TenantId; task_id: I.TaskId; runtime_adapter_version: string;
  session_binding_id: I.SessionBindingId | null; environment_id: I.EnvironmentId | null;
  state: AttemptState; version: number; lease_epoch: number; lease_owner: string | null;
  lease_expires_at: Timestamp | null; retry_reason: string | null; recovery_mode: RecoveryMode | null;
  started_at: Timestamp | null; ended_at: Timestamp | null;
}

export interface SessionBinding {
  id: I.SessionBindingId; tenant_id: I.TenantId; native_session_ref: string; runtime_version: string;
  workspace_id: I.WorkspaceId | null; checkpoint_ref: string | null; recovery_mode: RecoveryMode;
}

export interface Environment {
  id: I.EnvironmentId; tenant_id: I.TenantId; provider: string; provider_ref: string | null;
  state: EnvironmentState; version: number; trust_group: string;
}

export type AttentionType = "question" | "action_approval" | "authentication_handoff" | "blocked_work";

export interface AttentionRequest {
  id: I.AttentionRequestId; tenant_id: I.TenantId; task_id: I.TaskId; attempt_id: I.AttemptId;
  type: AttentionType; native_request_ref: string; action_hash: string | null; payload_ref: string;
  status: AttentionState; version: number; expiry: Timestamp; policy_version: string;
  authorized_responders: I.PrincipalId[];
}

export interface Decision {
  id: I.DecisionId; tenant_id: I.TenantId; request_id: I.AttentionRequestId; principal_id: I.PrincipalId;
  decision: "approve_once" | "deny" | "answer"; answer_ref: string | null; decided_at: Timestamp;
  request_version: number; dispatch_id: string | null; delivered_at: Timestamp | null; consumed_at: Timestamp | null;
}

export type ExecutionStatus = "recorded" | "dispatched" | "succeeded" | "failed" | "uncertain" | "denied";

export interface ActionOperation {
  id: I.ActionId; tenant_id: I.TenantId; attempt_id: I.AttemptId; target: Record<string, unknown>;
  normalized_input_hash: string; policy_version: string; grant_ref: I.GrantId | null;
  execution_status: ExecutionStatus; external_operation_ref: string | null; lease_epoch: number;
}

export interface Connection {
  connection_id: I.ConnectionId; tenant_id: I.TenantId; owner_principal: I.PrincipalId; provider: string;
  external_account_id: string | null; credential_ref: string | null; scopes: string[]; state: ConnectionState;
}

export interface Grant {
  id: I.GrantId; tenant_id: I.TenantId; connection_id: I.ConnectionId; task_id: I.TaskId | null;
  profile_id: I.ProfileId | null; capabilities: string[]; expires_at: Timestamp | null; revoked_at: Timestamp | null;
}

export interface Artifact {
  artifact_id: I.ArtifactId; tenant_id: I.TenantId; task_id: I.TaskId; attempt_id: I.AttemptId;
  uri: string; digest: string; classification: Classification; source_revision: string;
}

export interface Verification {
  verification_id: I.VerificationId; tenant_id: I.TenantId; task_id: I.TaskId; attempt_id: I.AttemptId;
  result_revision: string; verifier: "platform" | "agent"; result: "pass" | "fail" | "error";
  criteria_results: { criterion: string; result: "pass" | "fail" | "not_run" }[];
}

export interface Handoff {
  handoff_id: I.HandoffId; tenant_id: I.TenantId; parent_task_id: I.TaskId; child_task_id: I.TaskId;
  owner: I.PrincipalId; deadline: Timestamp | null; context_refs: string[];
}
