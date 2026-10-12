# ADR-0012 (DRAFT): Adopt OCC (Option A) or move to the lighter route (Option B)

- Status: **Draft — evidence missing; no decision**
- Spec: §29 phase 0 exit gate, §40, §42; ADR-0011

## Evidence so far
- Q1 FAIL on this host for an environmental reason (no kernel `ip_set` => no NetworkPolicy enforcement); no model credential.
- Documented (not observed) constraints of Option A with dedicated Codex:
  - OCC has no public session/turn/approval API; the product must speak the native gateway protocol behind an OCC service-key route.
  - One Harness per AgentRevision serves all sessions: per-task isolation requires one Agent (revision) per task/trust group.
  - No native resume after gateway restart on the pinned Codex plugin.
  - Kubernetes + Envoy Gateway + cert-manager + enforcing CNI are hard prerequisites; local setup needed 5 workarounds here.

## Missing evidence before deciding
1. Q1 PASS on a capable host with a real key: Agent ready and a bounded model turn.
2. Q2: approval requested -> product resolve -> denied command not executed; approved continues; question path for Codex.
3. Q3: product + gateway restart while waiting; stale approval rejected.
4. Q5: cancel stops the turn; stopAgent/cleanup observed; orphan check.
5. Q6: same slice on E2B (Option B) for cost/complexity comparison.
6. A scoped (non-admin) authentication path for the product to the gateway.

## Leaning (not a decision)
If (6) has no supported answer and per-task isolation requires an Agent per task, Option B's operating cost
is likely lower. Decide after Q1–Q3 run on a capable host.
