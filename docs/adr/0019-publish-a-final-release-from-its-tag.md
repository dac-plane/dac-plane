# ADR-0019: Publish a final release from its tag

- Status: Accepted
- Date: 2026-11-03
- Issue: [#95](https://github.com/dac-plane/dac-plane/issues/95)

## Context

[ADR-0018](0018-release-versioning-and-gate.md) ended the release gate with a draft that a person publishes, and the
first release also waited for a person to approve the `release` environment. Making `v0.1.0` showed that these steps
add waiting and little judgment: every check they could inform has already run, and the maintainer has already decided
by signing the tag.

## Decision

This narrows step 7 of ADR-0018's gate. Pushing a signed final tag is the decision to release. The `release` environment
has no required reviewer, and when the gate passes, the workflow publishes the GitHub Release itself. `-rc.N` rehearsals
still end in a draft prerelease.

## Consequences

- A release is available as soon as the gate passes, with no one needing to be present.
- No person reads the release before it is public. What protects it is the signed tag, the gate and any rehearsal.
- A release that passes the gate and is then found to be wrong cannot be held back. It gets the next patch number.
- A package new to the repository is still private until an owner opens it. Adding an image remains a manual step.

## Alternatives considered

- **Keep the environment approval and publish automatically:** rejected. It still needs someone present mid-run.
- **Keep the draft and remove the approval:** rejected. A draft still needs someone to publish it.
