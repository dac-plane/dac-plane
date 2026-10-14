# ADR-0005: Native approval authority plus product projection

- Status: Proposed
- Spec: §13, §33, §37 (approval integration rule)

## Decision
The native execution owner keeps final authority over whether a request is still eligible. The product
persists a projection (attention_request), binds approvals to a canonical action hash, request version,
principal, policy version and expiry, and delivers decisions via the native supported method. A stored
answer cannot resurrect expired/superseded native authority. Implemented in this repo: attention state
machine, canonical action hash, and DB constraint "at most one accepted decision per request version".

## Qualification evidence
CanDoItAll's workflow external-request path blocked the governed executor and persisted responder
identity, authority scope, action and request version, authorization-policy fingerprint, a 900-second
response lifetime and idempotency state across restart. It rejected missing/insufficient authority,
stale versions and conflicting replay. This proves that a native runtime can supply final execution
authority. The product-side projection and canonical binding remain required because CanDoItAll failed
the broader CP7 lifecycle mapping and is not the adopted lifecycle owner.
