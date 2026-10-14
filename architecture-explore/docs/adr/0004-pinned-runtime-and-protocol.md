# ADR-0004: Pinned runtime and protocol

- Status: Open
- Spec: §16, §37, §42 (Q2, Q3)

## Decision direction
Select the first runtime independently of the rejected OCC/OpenClaw deployment topology. Prefer a native
structured API or a negotiated, pinned ACP version; a supported gateway may still qualify as a runtime
adapter if it passes the same capability tests. Every adapter publishes a capability manifest
(`packages/product-api/schemas/capability-manifest.schema.json`) with tier A/B/C. A Tier C adapter must
not advertise action-level approvals (enforced in `capabilities.ts`).

## Open — needs Bryan / qualification
- Which runtime is the first pinned runtime (Codex, Claude Code, OpenCode, Grok Build via ACP, other)?
- Account authentication and licensing terms for that runtime.
- Exact version, transport and observed conformance results. OCC's dedicated Codex failure is recorded,
  but no independent runtime adapter has qualified yet.
