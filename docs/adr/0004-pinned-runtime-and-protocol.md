# ADR-0004: Pinned runtime and protocol

- Status: Open
- Spec: §16, §37, §42 (Q2, Q3)

## Decision direction
Reuse the OpenClaw Gateway / supported Codex harness path first; otherwise a native structured API or a
negotiated, pinned ACP version. Every adapter publishes a capability manifest
(`packages/product-api/schemas/capability-manifest.schema.json`) with tier A/B/C. A Tier C adapter must
not advertise action-level approvals (enforced in `capabilities.ts`).

## Open — needs Bryan / qualification
- Which runtime is the first pinned runtime (OpenClaw+Codex, Grok Build via ACP, other)?
- Account authentication and licensing terms for that runtime.
- Exact version, transport and observed conformance results. None recorded yet.
