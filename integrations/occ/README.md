# integrations/occ (WP03) — not started

Public-API mapping to OpenClaw Enterprise / OCC: ProductTenant -> admitted Namespace, ProductProfile ->
admitted Configuration/Agent, Attempt -> exact AgentRevision + native session/turn (spec §36).
Rules: no writes to OCC private tables, no new Compute controller, preserve original actor (ADR-0002, 0010).
Blocked on Q1 (OCC real deployment) and the Option A/B choice (ADR-0001).
