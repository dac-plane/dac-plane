# ADR-0005: Native approval authority plus product projection

- Status: Proposed
- Spec: §13, §33, §37 (approval integration rule)

## Decision
The native execution owner keeps final authority over whether a request is still eligible. The product
persists a projection (attention_request), binds approvals to a canonical action hash, request version,
principal, policy version and expiry, and delivers decisions via the native supported method. A stored
answer cannot resurrect expired/superseded native authority. Implemented in this repo: attention state
machine, canonical action hash, and DB constraint "at most one accepted decision per request version".

## Open
Whether OpenClaw exec approvals cover question-type requests and restart reconciliation (Q2, Q3).
