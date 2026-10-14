# ADR-0006: Product backend stack

- Status: Accepted
- Spec: §10, §26

## Decision
TypeScript (Node.js 20+) for the product backend and contracts, as named in spec §26 ("Small TypeScript
service"). PostgreSQL for product task state (§10). Contracts are published as JSON Schema (2020-12) and
OpenAPI 3.1 during phase 0 (§2 "How to use"). Tests: vitest. Repository layout follows §26
"Initial repository additions".
