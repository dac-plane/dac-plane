# ADR-0010: One lifecycle owner per resource

- Status: Accepted
- Spec: §40 "Single owner for each kind of state"

The selected product control plane owns task outcome and attention. A qualified foundation may own a
specific lifecycle record only when ADR-0013 mapping proves that the product can adopt it without a
second authority. Runtime owns native session/turn and action eligibility. Environment provider owns
compute lifecycle. Other services may observe or request transitions but never concurrently reconcile
the same resource. The product `environment` table is an observation/correlation record, not a second
controller.
