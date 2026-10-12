# ADR-0001: Composition over an evaluated foundation

- Status: Proposed
- Spec: §3, §8, §35, §40, §41

## Context
v1.1 replaces the v1.0 greenfield assumption. Agent deployment, IAM, audit, harness and sandbox lifecycle
are reuse candidates. OpenClaw Enterprise / OCC is the first foundation to evaluate (Option A). A lighter
existing-runtime + one managed sandbox composition is the fallback (Option B).

## Decision
Build only the product-specific task/attention/result layer and thin adapters. Do not build a deployment
controller, VMM fleet, agent harness, vault, chat transport or workflow platform without a demonstrated
gap (spec §41 "Do not build yet").

## Open
Choice between Option A and B depends on qualification experiments Q1-Q5 (spec §42). None have been run.
