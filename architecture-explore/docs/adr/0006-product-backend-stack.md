# ADR-0006: Product backend stack

- Status: Superseded by [`docs/adr/0001-control-plane-language.md`](../../../docs/adr/0001-control-plane-language.md)
- Spec: §10, §26

## Decision
This exploration originally selected TypeScript (Node.js 20+) for the product backend. The active
product decision now selects Go while retaining PostgreSQL, JSON Schema 2020-12 and OpenAPI 3.1.
