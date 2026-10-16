# Implementation plan: issue #16 artifacts, result revisions and reviews

Tracker: [GitHub issue #16](https://github.com/AssahBismarkabah/dac-plane/issues/16) (epic #11)

## Objective

Define how evidence enters the product and how results are judged: staged artifact bytes admitted
through a fenced manifest commit (`committed | retry | fenced`), immutable ResultRevisions that
reference only committed artifacts, and append-only ResultReviews whose effective disposition
drives the Task with expected-version conflict handling. Pure Go plus JSON Schema 2020-12; no
object store, persistence or UI.

## Sources

- Baseline ownership rule 6: artifact bytes may live outside PostgreSQL, but artifact identity,
  provenance, integrity metadata and result association remain product records. Container view:
  "store or authorize upload, verify digest".
- Review invariants: artifact bytes are integrity checked; metadata and result lineage are
  committed by the product; registration requires a verified committed manifest; retry and fenced
  outcomes create no Artifact or ResultRevision. Contract-freeze items 3 and 5.
- ADR-0003 "Fenced artifact admission": only a committed manifest with a verified digest may
  register an Artifact or be referenced by a ResultRevision; retry records no admission and may
  repeat under the stable operation identity; fenced rejects a stale writer without changing the
  winning manifest.
- Baseline vertical slice: record artifacts and a result revision, then accept, reject or request
  rework. Task success is reachable only through review acceptance (#39).
- Exploration evidence: Plane-Bot store manifest commit `COMMITTED/RETRY/FENCED` with checksum
  failure codes (DEEP-DIVES-01, DAC-PLANE-MAPPING 7); historical spec evidence items (code
  change, test result, visual proof, external action, agent claim) and review endpoint binding
  result version and reviewer authority.

## Design (`internal/lifecycle`)

- `ArtifactManifest`: what a writer claims it uploaded: artifact id, tenant, task, attempt, the
  stable commit operation (manifest identity), kind, declared digest and size, media type,
  producing provider, optional source revision, and the writer epoch (fence).
- `StoredObject`: the store's observation of the bytes: present, computed digest and size.
- `AdmitArtifact(manifest, stored, currentEpoch, existing)` returns an `ArtifactCommitOutcome`:
  - `fenced` when the writer epoch is older than the attempt's current epoch, or a different
    manifest already won this artifact; the winner is returned unchanged;
  - `retry` when bytes are missing or the digest or size disagree (with a typed reason); nothing
    is registered and the same operation may try again;
  - `committed` only when the store's digest and size equal the declaration; the `Artifact`
    records exact digest, size, manifest operation, provenance and writer epoch. Re-committing
    the same manifest is idempotent.
  Each outcome has an audit form (`stale_writer` and `admission_retry` for refusals).
- `ResultRevision`: immutable; number per task, one or more artifacts, optional previous revision
  for rework lineage. `NewResultRevision` fails unless every referenced artifact is a valid
  committed Artifact of the same task, and a previous revision belongs to the task with the
  preceding number.
- `ResultReview`: append-only; binds tenant, task, exact revision, reviewer, disposition
  (`accept | request_changes | reject`) and the task version the reviewer saw.
  `ApplyReview(task, current revision, review, reviewers)` checks tenant, task, that the review
  names the current revision, and that the reviewer is the task owner or an authorized reviewer,
  then applies the disposition to the Task at the review's expected version: concurrent
  reviews conflict, request changes queues new work and never mutates the revision.

## Ordered tasks

1. Identifiers (Artifact, Revision, Review), audit kinds and rejection codes.
2. Manifest, stored object, admission outcomes and Artifact; outcome table tests.
3. ResultRevision and ResultReview with lineage, authority and conflict tests.
4. JSON Schemas and agreement tests (both coverage guards require them).
5. Mutation checks and the full CI set, including uncached integration tests.

## Verification

```sh
go test ./internal/lifecycle/... -run 'Artifact|Result|Review'
go test -race ./internal/lifecycle/...
go test -count=1 -tags=integration ./...
```

## Boundaries

- No object storage, presigned uploads, retention policy, verification pipeline or review UI.
- No classification labels yet; no active document defines them.
- The issue names `internal/domain`; these records live in `internal/lifecycle` like #14 and #15.
