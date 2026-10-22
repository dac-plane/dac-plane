# Issue #66 — Decide the Surface API transport, versioning and actor authentication

Tracker: https://github.com/dac-plane/dac-plane/issues/66
Branch: `docs/66-surface-api-adr`, from `main` at `034fa93`.
Parent: #65 (M2: Deployable control plane).

## Direction

Record ADR-0007 as a decision, not an implementation. Surfaces are clients of one deployed control
plane; the Surface API is a thin transport over the existing transport-neutral command boundary in
`internal/application`. Inputs already decided elsewhere are reused, not reopened:

- ADR-0001 fixes public contracts as OpenAPI 3.1 and JSON Schema 2020-12.
- ADR-0002 places the Surface API, authentication and actor authorization inside the one
  control-plane application, with no handler bypassing lifecycle commands.
- The review invariants require every mutating command to be authenticated, authorized,
  idempotent and version checked.

Decisions confirmed with the maintainer on 2026-10-22:

- Operator model: neutral to the operator, self-hosted first. The operator configures identity;
  a managed offering later uses the same artifact.
- Identity: adopt the historical specification's position. People authenticate through an external
  OpenID Connect provider; machine surfaces use scoped service credentials from the same provider;
  organization SSO is later. dac-plane stores no passwords.

Evidence used: `architecture-explore/plane-bot-explore/notes/MASTER-RECORD.md` §5 items 11, 14 and 15
and `DEEP-DIVES-01.md` §1 (command-oriented, mostly unary surface, streaming only where needed,
client nonces, admin twins; vendor trust topology not adopted), and
`architecture-explore/docs/spec/SPEC.md` (platform and workload identity rows, tenant definition,
section 15 public API, event replay with cursor).

## Implementation

- [x] Confirm the operator model and identity position before drafting.
- [x] Read ADR style (0001, 0005, 0006), the command envelope (`internal/application/application.go`),
  identity contracts (`internal/identity`), and the durable event table.
- [x] Draft `docs/adr/0007-surface-api-transport-and-authentication.md` with status Proposed.
- [x] Resolve the three open questions with the maintainer (2026-10-22): owner-scoped reads with a
  narrow tenant-reader permission; trusted provisioning command as the only way to create tenants,
  principals, bindings and permissions; no time-based cursor window, reset on invalid or
  other-generation cursors, retention deferred to its own decision.
- [ ] Maintainer review of the complete draft.
- [ ] Link ADR-0007 from `docs/architecture/README.md` once accepted.
- [ ] Open the PR with the governance template; set status Accepted on approval.

## Verification

- `git diff --check`
- Every relative link in the ADR resolves to a file on this branch.
- `just test-contracts` is unaffected (no schema change in this issue).

## Findings carried to implementation issues

- `durable_event` is keyed by UUID with no monotonic per-tenant sequence, so it cannot serve as a
  resumable stream cursor yet. #70 must add an ordered sequence before the event stream exists.
- The application layer exposes few reads (`GetSubmission`, `GetReviewedResult`, `GetMigration`,
  `GetGovernedAction`), mostly keyed by nonce. Task, attempt and attention queries must be added as
  application functions in #70, not composed in handlers.
- Mapping an external identity to a principal needs a new stored binding record with a schema and
  agreement test; that belongs to #71, together with the tenant-reader permission record and the
  trusted provisioning command.
- The event cursor must carry a stream generation as well as the sequence, so a backup restore or
  compaction yields a reset instead of skipped events (#70).
- No retention policy exists for durable events; the minimum resumable window waits for a data
  retention decision.

## Remaining boundaries

This issue decides no adapter transport (#67), deployment or TLS placement (#68), role model or team
sharing beyond the tenant-reader permission, data retention, rate limiting, or web-workspace browser
concerns such as CORS and cookie sessions.
