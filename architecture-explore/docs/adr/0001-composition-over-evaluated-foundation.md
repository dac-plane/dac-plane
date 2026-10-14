# ADR-0001: Composition over an evaluated foundation

- Status: Accepted — composition boundary retained; foundation search closed
- Spec: §3, §8, §35, §40, §41

## Context
v1.1 replaces the v1.0 greenfield assumption. Agent deployment, IAM, audit, harness and sandbox lifecycle
are reuse candidates. OpenClaw Enterprise/OCC was the first foundation evaluated (Option A). A lighter
existing-runtime plus one managed sandbox composition was the specified fallback (Option B).

## Decision
Build only the product-specific task/attention/result layer and thin adapters. Do not build a deployment
controller, VMM fleet, agent harness, vault, chat transport or workflow platform without a demonstrated
gap (spec §41 "Do not build yet").

## Qualification outcome
Choice between Option A and B originally depended on qualification experiments Q1-Q5 (spec §42). Q1
failed for stock OCE; see ADR-0012. ADR-0013 rejected the other candidates. CanDoItAll passed provider,
authority, restart and cancellation tests but failed the hard task/attempt budget mapping gate. No
existing foundation qualified, so the project proceeds with the minimum product-owned lifecycle core.
