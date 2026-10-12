/** Runtime capability manifest and tiers (spec §16, §33). */
export type CapabilityTier = "A" | "B" | "C";

export interface RuntimeCapabilities {
  structured_events: boolean;
  permission_callbacks: boolean;
  follow_up: "queued" | "interrupt" | "unsupported";
  cancel: "native" | "native_then_process_stop" | "process_stop" | "unsupported";
  recovery: "exact_resume" | "restart_from_artifacts" | "unsupported";
  usage: "full" | "partial" | "none";
  concurrent_turns: boolean;
  secure_auth_handoff: boolean;
  native_resume_after_gateway_restart?: boolean;
}

/** Smallest unit with its own execution environment (spike finding, ADR-0012). */
export type IsolationScope = "per_task" | "per_session" | "per_agent_revision" | "shared";

export interface CapabilityManifest {
  adapter: string;
  runtime: string;
  protocol: string;
  tier: CapabilityTier;
  isolation_scope: IsolationScope;
  capabilities: RuntimeCapabilities;
  conformance_report_ref: string | null;
}

/** Tier implied by declared capabilities. */
export function deriveTier(c: RuntimeCapabilities): CapabilityTier {
  const cancels = c.cancel !== "unsupported";
  if (c.structured_events && c.permission_callbacks && cancels && c.recovery === "exact_resume") return "A";
  if (c.structured_events && c.permission_callbacks) return "B";
  return "C";
}

export function manifestProblems(m: CapabilityManifest): string[] {
  const p: string[] = [];
  const derived = deriveTier(m.capabilities);
  if (tierRank(m.tier) > tierRank(derived)) p.push(`declared tier ${m.tier} exceeds capabilities (tier ${derived})`);
  if (m.tier === "C" && m.capabilities.permission_callbacks) {
    p.push("Tier C adapter must not advertise action-level approvals");
  }
  if (m.isolation_scope !== "per_task") p.push(`isolation scope ${m.isolation_scope}: tasks share an execution environment (ADR-0003 needs a trust group)`);
  if (m.conformance_report_ref === null) p.push("no conformance report: capabilities are unverified");
  return p;
}

/** §16: a Tier C adapter must not advertise action-level approvals. */
export function supportsActionApprovals(m: CapabilityManifest): boolean {
  return m.tier !== "C" && m.capabilities.permission_callbacks;
}

function tierRank(t: CapabilityTier): number {
  return t === "A" ? 3 : t === "B" ? 2 : 1;
}
