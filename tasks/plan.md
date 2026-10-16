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
`AttemptID`, `ProviderID`, `OperationID`, `GrantID`) without duplicated code; later
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

The model follows the Plane-Bot local authorization evaluator
([DEEP-DIVES-03 §3](../architecture-explore/plane-bot-explore/notes/DEEP-DIVES-03.md)) and the
historical specification's approval binding (SPEC §13, §18), adopted through ADR-0003.

- `ActionClass`: an opaque, validated action name (for example `message.send`). The catalogue of
  classes is provider-specific and not frozen here.
- `ScopeClass` (`global | setting | subject | call`) is a property of the action, fixed by the
  action catalogue, not a breadth a grant chooses. `Scope` pairs the class with a target that
  setting and subject classes require and the others forbid.
- `Permission` lattice `never < ask < always`. `Policy` holds the tenant's `Settings` and the
  administrative `Ceiling` per action class; the effective permission is the lower of the two,
  and a class missing from either is `never`. Policy changes advance `Generation`.
- `DenyPolicy`: a never rule for one named setting or subject of an action. A never rule for a
  whole action class is `PermissionNever`, so there is one way to express each.
- `ActionRequest`: the effective (post-hook) action: tenant, attempt, provider, operation, action
  class, the action's catalogue scope, and a SHA-256 digest of the effective input.
- `AuthorityGrant`, always bound to one Attempt (fail closed; tenant-wide standing authority is
  policy, not a grant):
  - `allow_once`: a one-time approval of one operation and one exact input digest, for an action
    of any scope class, retired by its first use. A changed input needs a new approval.
  - `standing`: covers the action within a scope equal to the action's catalogue scope and
    target. Call-scoped actions cannot have standing grants. Requires effective `always`.
  - The source attribution (`one_time_approval | standing_grant`) is derived from the mode
    rather than stored twice; `granted_by` records the approving principal.
- `Authorize(req, grant, policy, now)`: a pure function. Never rules are checked before the grant
  and win over any grant, matching deny-first evaluation in comparable agent permission systems.
  It returns an `AuthorityUse` identified by the requested operation (ADR-0003). For an allowed
  `allow_once` it also returns the retired grant; the caller persists both atomically in #20.
  Errors are reserved for malformed inputs; a denial is an outcome, not an error.
- `IssueGrant(...)`: validates a new grant against generation, permissions and never rules.

Denial reasons are a closed enum (no grant, cross-tenant, wrong attempt, wrong audience, wrong
action, wrong operation, input mismatch, scope mismatch, revoked, retired, not yet valid,
expired, stale generation, above ceiling, denied by policy) so audit and simulators can assert
exact outcomes.

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
3. Scope, permission lattice, never rules and grant issuance with table tests.
4. `Authorize` and `AuthorityUse`, with one table test per denial reason and per acceptance
   criterion in #13 (allow-once reuse, standing exact-scope, permission clamping, source
   attribution).
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
