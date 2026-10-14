/**
 * Opaque identifiers (spec §5 "Identity rules"). Task, session, attempt and environment IDs are
 * separate types. Native runtime IDs are adapter mappings and are never authorization tokens.
 */
declare const brand: unique symbol;
export type Id<K extends string> = string & { readonly [brand]: K };

export type TenantId = Id<"tenant">;
export type PrincipalId = Id<"principal">;
export type ProjectId = Id<"project">;
export type WorkspaceId = Id<"workspace">;
export type ProfileId = Id<"profile">;
export type TaskId = Id<"task">;
export type AttemptId = Id<"attempt">;
export type SessionBindingId = Id<"session_binding">;
export type EnvironmentId = Id<"environment">;
export type AttentionRequestId = Id<"attention_request">;
export type DecisionId = Id<"decision">;
export type ActionId = Id<"action">;
export type ConnectionId = Id<"connection">;
export type GrantId = Id<"grant">;
export type ArtifactId = Id<"artifact">;
export type VerificationId = Id<"verification">;
export type EventId = Id<"event">;
export type CommandId = Id<"command">;
export type HandoffId = Id<"handoff">;

export const ID_PREFIX = {
  tenant: "ten", principal: "prn", project: "proj", workspace: "ws", profile: "prof", task: "task",
  attempt: "att", session_binding: "sb", environment: "env", attention_request: "req", decision: "dec",
  action: "act", connection: "conn", grant: "grt", artifact: "art", verification: "ver", event: "evt",
  command: "cmd", handoff: "hof",
} as const;

export type IdKind = keyof typeof ID_PREFIX;

export function newId<K extends IdKind>(kind: K, random: () => string = defaultRandom): Id<K> {
  return `${ID_PREFIX[kind]}_${random()}` as Id<K>;
}

export function isId(kind: IdKind, value: string): boolean {
  return new RegExp(`^${ID_PREFIX[kind]}_[A-Za-z0-9]{8,64}$`).test(value);
}

function defaultRandom(): string {
  return globalThis.crypto.randomUUID().replace(/-/g, "");
}
