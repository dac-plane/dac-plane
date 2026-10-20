# ADR-0004: Restrict migration administration and separate transfer evidence

- Status: Accepted
- Date: 2026-10-20

## Context

ADR-0003 adopts durable migration passes, holds and preview/execute administration,
but does not define a product administrator permission. A principal record proves
tenant identity, not permission to administer providers. The existing pass and
maintenance claim repositories also do not compose an authenticated command path.
Exploration's internal preview/execute methods supply evidence for that boundary,
not a reusable provider-transfer engine or an adopted role hierarchy.

## Decision

Require a narrow persisted tenant/principal harness-administrator permission,
provisioned or revoked through trusted setup. There is no self-grant command,
caller-supplied reviewer/admin list, general RBAC framework or new identity service.
Current permission is checked under its row lock before new commands and exact
nonce replays; revocation therefore serializes with mutation and replay retrieval.

Expose transport-neutral create, preview, hold, release, execute and cancel commands
with the existing command nonce and expected pass version. Audit creation and
transitions atomically with command results. Preview locks both live qualifications
in provider-ID order and records their versions and server time. Execute requires
that exact snapshot within five minutes. Hold/release clears it. Existing domain
rules permit cancellation only before execution; in-flight transfer handling is #60.

One tenant-wide slot serializes administrative intent. Its reusable maintenance
claim is anchored to the first source provider only to satisfy the existing claim
identity; it is an administrative resource, not an active execution binding or
provider-specific compute claim. Cancelling a pre-execution pass frees the slot for
a later pass without colliding with the claim's permanent natural-key uniqueness.

The worker explicitly acquires scheduled claims linked to executing passes. Initial
acquisition, takeover and renewal join the same scope lock order and require the
linked pass and preview qualifications to remain authoritative. Reaper selection
and locked mutations both use PostgreSQL time. Legacy unlinked claims retain their
existing behavior. Lock order is permission (commands only), tenant advisory lock,
slot, pass, sorted qualifications, claim. Qualified live status is necessary but
not proof of physical migration success.

An execute command records intent only. Administration exposes no complete/fail
outcome method and makes no external provider calls or binding changes. Actual
source quiescence, target preparation, cutover, cleanup, outcome evidence and
in-flight cancellation belong to [#60](https://github.com/dac-plane/dac-plane/issues/60).
The #30 freeze must account for that ticket; administrative acceptance does not
stand in for executed migration evidence.

Migration 00007 is additive. Old pass/claim records remain readable; no historical
preview or administrator evidence is fabricated. Downgrade refuses any populated
new administration or permission table rather than silently removing authority
or previews.

## Alternatives considered

Giving every authenticated principal administration authority conflates identity
with permission. Caller allowlists or system-actor command fields are forgeable.
A general role system is larger than the adopted simulator scope. Keeping only
repository tests would leave the required authenticated boundary unproved.
Implementing a provider-transfer engine here would combine two separately tracked
concerns and invent completion evidence before executing the physical protocol.

## Consequences

Administration is deny-by-default and replay revocation is stricter than ordinary
historical command retrieval. Trusted provisioning remains an operational boundary,
not a production identity integration. Executing passes intentionally remain pending
until #60 implements evidence-backed settlement. No production provider or transport
is selected, and no accepted ADR or exploration source is changed.
