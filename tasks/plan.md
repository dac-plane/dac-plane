# Implementation plan: issue #13 identity and scoped authority contracts

Tracker: [GitHub issue #13](https://github.com/AssahBismarkabah/dac-plane/issues/13) (epic #11)

## Objective

Define the transport-neutral identity and delegated-authority contracts that every command and
provider interaction carries, as pure Go types and decision functions with no persistence. The
semantics come from the "Scoped action authority" and "Action interception" sections of
[ADR-0003](../docs/adr/0003-provider-contract-semantics.md) and the review invariants.

## Design

### Identifiers (`internal/ids`)

Opaque, typed identifiers shared by identity (#13) and domain (#14) so neither package imports the
other. One generic `ID[K]` with a phantom kind gives distinct types (`TenantID`, `PrincipalID`,
`AttemptID`, `ProviderID`, `OperationID`, `GrantID`, `PolicyID`) without duplicated code; later
issues add kinds as records need them. Each wraps a UUIDv7 (time-ordered, maps to a PostgreSQL
`uuid` column in #19). The zero value is invalid and does not marshal; parsing accepts only the
canonical lowercase UUIDv7 form so Go and JSON Schema agree on one pattern.

### Identity (`internal/identity`)

- `Principal`: tenant, principal ID and kind (`human`, `service`). A principal belongs to exactly
  one tenant; a person in two tenants has two principals. Providers are not principals; they are
  audiences of authority.
- `AuthorizationContext`: the authenticated tenant and principal for one command, independent of
  transport. `RequireTenant(owner TenantID)` rejects cross-tenant use with a typed error.
- No credential material in any type. Credential references are left to the issue that
  introduces a credential broker; this issue only guarantees no record can carry a secret.

### Authority (`internal/identity`)

- `ActionClass`: an opaque, validated action name (for example `fs.write`). The catalogue of classes
  is provider-specific and not frozen here.
- `Scope`: class `global | setting | subject | call` with an optional value. Breadth is ordered
  `call < subject < setting < global`.
- `Scope` values: `global` has none; `setting` and `subject` name the setting or subject; `call`
  has none and binds through the grant's operation.
- `AuthorityGrant`: tenant, attempt, audience provider, action class, scope, mode
  (`allow_once | standing`), granting principal, policy generation, issued/expires, revoked and
  retired times. Every grant is bound to one Attempt (fail closed; tenant-wide standing grants are
  not modelled). `allow_once` requires `call` scope and an operation; `standing` may not use
  `call` scope. The source attribution (`one_time_approval | standing_grant`) is derived from the
  mode rather than stored twice.
- `DenyPolicy` (`never`): tenant, action class and scope. It blocks both issuance and use of any
  overlapping grant.
- `PermissionCeiling`: per tenant, the broadest scope class allowed per action class. Neither
  issuance nor use may exceed it.
- `ActionRequest`: the effective (post-hook) action: tenant, attempt, provider, operation, action
  class, subject.
- `Authorize(req, grant, policy, now)`: a pure function that checks tenant, deny policies,
  attempt, audience, action class, scope and operation, revocation, retirement, expiry, policy
  generation and ceiling, then returns an `AuthorityUse` (allowed or denied with a typed reason,
  the grant source and the granting principal). An `AuthorityUse` is identified by the requested
  operation, per ADR-0003. For an allowed `allow_once` it also returns the retired grant; the
  caller persists both atomically in #20. Errors are reserved for malformed inputs; a denial is
  an outcome, not an error.
- `IssueGrant(...)`: validates a new grant against ceiling and deny policies.

Denial reasons are a closed enum (no grant, cross-tenant, wrong attempt, wrong audience, wrong
operation, wrong action, scope mismatch, expired, revoked, retired, stale generation, above
ceiling, denied by policy) so audit and simulators can assert exact outcomes. An action class
missing from the ceiling is above ceiling (fail closed).

### JSON Schema (`contracts/jsonschema/`)

JSON Schema 2020-12 documents for the identifier pattern, principal, action request, policy,
authority grant and authority use. Tests compile them with `santhosh-tekuri/jsonschema/v6`
(format assertions on) and prove agreement: Go-marshalled valid records validate, a shared set of
invalid documents is rejected by both the schema and Go validation, and every schema enum equals
the Go constant set.

## Ordered tasks

1. `internal/ids`: typed UUIDv7 identifiers with parse/validate and tests.
   Verify: `go test ./internal/ids/...`
2. `internal/identity`: principal, authorization context and cross-tenant rejection with tests.
   Verify: `go test ./internal/identity/...`
3. Scope, ceiling, deny policy and grant issuance with table tests.
4. `Authorize` and `AuthorityUse`, with one table test per acceptance criterion in #13 (allow-once
   reuse, standing exact-scope, expired, revoked, wrong audience/operation, stale generation,
   above ceiling, source attribution).
5. JSON Schemas and the agreement tests above.
6. A test that marshals every identity and authority record and asserts that it has no
   credential-bearing fields.
7. Full CI verification.

## Verification

```sh
go test ./internal/ids/... ./internal/identity/...
go test -race ./internal/identity/...
go vet ./...
test -z "$(gofmt -l cmd internal)"
```

## Boundaries

- No persistence, migrations, HTTP, login or external identity provider.
- No action-class catalogue and no production credential broker.
- Hook interception and the dispatch transaction are #17 and #20; this issue supplies the pure
  authorization decision they call.
