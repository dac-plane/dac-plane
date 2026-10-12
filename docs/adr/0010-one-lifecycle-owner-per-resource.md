# ADR-0010: One lifecycle owner per resource

- Status: Proposed
- Spec: §40 "Single owner for each kind of state"

Product owns task outcome and attention. OCC (if adopted) owns Agent resources/revisions. Runtime owns
native session/turn and action authority. Sandbox provider owns compute lifecycle. Other services may
observe or request transitions but never concurrently reconcile the same resource. The product
`environment` table is an observation/correlation record, not a second controller.
