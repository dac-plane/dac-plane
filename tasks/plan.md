# Issue #93 — Qualify the first artifact byte store

Tracker: https://github.com/dac-plane/dac-plane/issues/93
Branch: `test/93-artifact-byte-store`, from `origin/main` at `67d078a`.
Parent: #75, last open item. Blocks M3 (#94). Decision: a new ADR-0017.

## Where this starts

The staged protocol from #26 ([artifact-results-26.md](../docs/architecture/artifact-results-26.md)) is
implemented and is the contract this issue qualifies against:

- `authorize_upload` returns an opaque capability, at most 4096 bytes, held only in memory.
- The runtime does a single `PUT` with `Content-Type` and `Content-Length` and nothing else
  ([adapter.go:800](../adapters/codex/adapter.go)).
- `stat_object` is diagnostic only.
- `commit_manifest` must hash and freeze the same object in one atomic step and return the manifest,
  observed digest and size, an immutable object version and a commit time.
- `retry` is not final; `committed` and `fenced` are.
- All four operations must replay exactly. Replaying authorization must return a capability that
  still works.

Today the deployed store is the simulator in artifact-only mode (`deploy/compose/compose.codex.yaml`,
`cmd/dac-sim-adapter/artifact_upload.go`). No real store sits behind it.
`architecture-explore/` has no research on byte stores. It has only the plane-bot pattern: multipart
writes plus a `COMMITTED / RETRY / FENCED` manifest commit
(`plane-bot-explore/notes/DAC-PLANE-MAPPING.md`). That is the shape #26 already adopted.

## Candidate comparison (primary sources, checked 2026-11-03)

| Candidate | Scoped upload | Overwrite refusal | Immutable versions | Retention / delete | Disposition |
|---|---|---|---|---|---|
| AWS S3 | SigV4 presigned PUT per key and method, signed `Content-Type`, expiry | `If-None-Match: *` on PutObject, CompleteMultipartUpload, CopyObject; can be enforced by bucket policy | Versioning; Object Lock governance/compliance | Lifecycle expiry; a locked version cannot be removed by lifecycle; governance bypass needs `s3:BypassGovernanceRetention` | Strongest semantics. Managed, so it cannot be pinned. No AWS credential in the local `.env`. |
| SeaweedFS (Apache-2.0) | SigV4 presigned PUT | `If-None-Match` on put, copy and delete | Versioning; Object Lock legal hold and retention | Lifecycle | Self-hostable and pinnable. Recent bugs found by others, listed below. |
| Ceph RGW | Presigned SigV4 | Conditional PUT (docs are thin) | Versioning; Object Lock | Lifecycle | Capable, but too heavy to run for a bounded qualification. |
| Garage | Presigned SigV4 | Not documented | No versioning, no Object Lock | Lifecycle only partly implemented | Rejected: cannot freeze a version. |
| MinIO CE | Presigned SigV4 | Yes | Yes | Yes | Rejected: maintenance mode since December 2025, source-only distribution since October 2025. |
| Cloudflare R2 | Presigned SigV4 | `If-None-Match` | No S3 Object Lock or lifecycle on PutObject | Uses vendor-specific bucket locks | Rejected for the first store. |
| RustFS | Presigned SigV4 | Yes | Versioning | Yes | Not chosen: 1.0.x, and a recent advisory let a writer with only PutObject overwrite an unlocked version (GHSA-gjph-xcgw-4p7w). |

Sources:
- AWS:
  - [conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
  - [enforcing them](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes-enforce.html)
  - [presigned URLs](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html)
  - [Object Lock considerations](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock-managing.html)
- Garage: [S3 compatibility](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility)
- R2: [S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api)
- SeaweedFS: [conditional writes](https://seaweedfs.com/blog/conditional-writes)

Findings that shape the design:

1. **A store-side checksum cannot be the integrity gate.**
   - AWS checks `x-amz-checksum-*` only when it arrives as a header. When it is hoisted into the query
     string, it is signed but not checked (LocalStack's AWS-recorded `test_presigned_url_v4_x_amz_in_qs`).
   - SeaweedFS ignored the presigned checksum algorithm in 4.19 (seaweedfs#9075).
   - Our runtime sends only `Content-Type` and `Content-Length`.
   - So commit hashes the bytes itself, from one pinned version. The store's own checksum is never
     trusted.
2. **Freeze by version, not by key.** A presigned PUT can be replayed until it expires. Commit therefore
   reads one staged `VersionId` and copies exactly that version to a final key. The copy uses
   `If-None-Match: *`, so the first commit wins. No capability is ever issued for the final key.
3. **SeaweedFS has had conformance bugs in exactly these features.** The qualification must observe each
   one, not assume it:
   - conditional writes with versioning, fixed in January 2026 (seaweedfs#8073)
   - `s3.bucket.create` silently dropping Object Lock and lifecycle on an existing bucket (seaweedfs#11450)
4. **SDK pin.** aws-sdk-go-v2 1.43.7 broke presigned GET by signing `X-Amz-Checksum-Mode`
   (aws-sdk-go-v2#3528). Set `ResponseChecksumValidation: WhenRequired`, pin exact versions, and test
   the presigned URLs with a plain `net/http` client, not the SDK.

Context7 (`/seaweedfs/seaweedfs` 4.51) confirms the following:
- CopyObject accepts `x-amz-copy-source-version-id` together with `If-None-Match: *` on the destination.
- Object Lock must be enabled when the bucket is created, and it turns on versioning.
- Lifecycle skips objects that are under retention.

These match the AWS behavior the design relies on. The spike still has to observe each one.

## Decision (taken 2026-11-03)

The pilot (#96) uses the bundled SeaweedFS profile. SeaweedFS is qualified first. AWS S3 follows with the
same adapter once an account exists.

### Recommendation as proposed

Build **one S3-API artifact adapter** and qualify **SeaweedFS** first, at an exact pinned image digest,
self-hosted in the reference Compose stack and in CI:

- It fits ADR-0009's self-hosted unit, and it can be pinned. A managed AWS backend cannot be.
- No AWS account is needed to land M2.
- The adapter uses only the S3 operations listed above. Running the same recipe against AWS S3 later is
  a configuration change plus a new evidence run, not new code.

The alternative is AWS S3 first, if you have an account for it. It has stronger semantics, but it adds
cost, a credential, and a backend that cannot be pinned.

## Deployment and support model

The artifact store is an operator-provided dependency, like PostgreSQL in ADR-0009. dac-plane ships an
adapter and a list of qualified stores. It does not ship or run the store as part of the product.

- **What "supported" means.** A store is supported only when the qualification recipe has passed against
  it at a recorded version, and an upgrade repeats that run. "S3-compatible" alone is not a claim.
  Stores that are not listed may work, but they are unsupported.
- **What the operator provides:**
  - the store itself (an AWS account, or their own SeaweedFS)
  - the two buckets, created as documented: Object Lock on the objects bucket, versioning, lifecycle rules
  - an adapter credential scoped as documented
  - a separate operator credential for deletion
  - backups, capacity and cost
- **What the adapter does at startup.** It checks versioning, Object Lock, the lifecycle rules and that its
  credential is not an admin. On any mismatch it refuses to serve. A misconfigured bucket therefore fails
  at deployment, not on the first commit.
- **Reference Compose stack.** ADR-0009 says it carries no artifact store until one is qualified. This
  issue adds an optional profile:
  - a SeaweedFS service at a pinned digest, with a persistent volume and TLS from the stack certificates
  - one-shot bucket provisioning that is safe to re-run
  - `dac-s3-adapter`

  That is the same pattern as PostgreSQL: bundled for the pilot, operated by the operator. An operator
  with AWS S3 or their own cluster leaves out the SeaweedFS service and sets the adapter's endpoint.
- **Configuration (environment or secret files):**
  - endpoint
  - region
  - the two bucket names
  - credential files
  - upload-link lifetime
  - the public endpoint used to sign upload links

## Adapter design (`adapters/s3`, `cmd/dac-s3-adapter`)

The adapter keeps no local state. Everything it needs after a restart lives in the store.

- **Two buckets.**
  - The staging bucket is versioned, with lifecycle rules and no lock.
  - The objects bucket has Object Lock with governance-mode default retention.
  - Reason: a default retention applies to every version in its bucket. If staging shared the locked
    bucket, every staging upload and fence update would be locked, and lifecycle could never expire them.
- **Keys:**
  - staging: `staging/{tenant}/{attempt}/{artifact}/{writer_epoch}` (staging bucket)
  - fence: `fences/{tenant}/{artifact}` (staging bucket)
  - final: `{tenant}/{artifact}` (objects bucket)
- **Replay (ADR-0012).**
  - Offer `operation.replay` only once the following hold.
  - Deliveries of the same operation are serialized in-process.
  - A replay with the same operation id but different arguments is refused, as the simulator refuses it.
    The authorize request digest is recorded on the fence object and the commit request digest in the
    receipt metadata, so the refusal still works after a restart.
- **Fencing mirrors `lifecycle.AdmitArtifact` and the simulator.**
  - A worker takeover keeps the writer generation (#26). Replaying the same commit therefore returns
    the recorded receipt, as `TestArtifactCrashAfterStoreCommitFencesOldWorker` expects.
  - `fenced` is returned only for a stale epoch or a different winning manifest.
- **The upload host is the one the runtime reaches.**
  - SigV4 signs the host, so presigning uses the public endpoint, not an internal alias.
  - The Codex uploader accepts only HTTPS or loopback HTTP (`validateUploadURL`). The SeaweedFS S3
    listener therefore serves TLS from the stack certificates.
- **`authorize_upload`:**
  1. Check the manifest's tenant and attempt against the request.
  2. Raise the fence object to the manifest's writer epoch, compare-and-swap with `If-Match`.
  3. Presign a PUT for the staging key, with signed `Content-Type` and an expiry of at most 15 minutes.
  4. A replay presigns the same key again.
- **`stat_object`:** HEAD and GET the current staging version, then hash and size it with a bound of
  manifest size plus one byte.
- **`commit_manifest`:**
  1. If the final object exists, read the receipt from its metadata. Return it if the manifest matches;
     otherwise return `fenced`.
  2. If the epoch is below the fence, return `fenced`.
  3. HEAD the staging key to get its `VersionId`. GET that version and hash it.
  4. If the bytes are missing, or the digest or size is wrong, return `retry` with that reason.
  5. Otherwise, CopyObject that version to the final key with `If-None-Match: *`. The copy's metadata
     carries the receipt: operation, manifest digest, committed time.
  6. On `412`, read the winner again.
  7. `object_version` is the final key's `VersionId`.
  8. The winner is the oldest version in `ListObjectVersions` order, read by `VersionId`, never the
     current version. The spike showed the adapter key can overwrite the current version.
- **Objects bucket policy:** deny `s3:DeleteObject` and `s3:DeleteObjectVersion` to everyone. This keeps
  delete markers out. Operator deletion is: remove the policy, then delete with governance bypass. A
  create-only `if-none-match` policy is not usable, because it also refuses the conditional copy.
- **Credentials:**
  - The adapter's own key may write staging and fences, copy to final, and delete staging only.
  - Deleting a final object takes a separate operator credential, never held by the adapter.
  - The adapter's key must not be a SeaweedFS admin identity, because admins always bypass governance
    retention (SeaweedFS wiki, S3 Object Lock and Retention).
  - The adapter authenticates to the control plane with the existing `DAC_ARTIFACT_STORE_CLIENT_ID` and
    secret-file pair (ADR-0008).
- **Retention:**
  - A lifecycle rule expires `staging/` and noncurrent versions.
  - Final objects get Object Lock governance retention.
  - Operator deletion is a documented procedure. The qualification observes what stat and a replay
    return afterwards.
- **Wire:** Built on `adapterkit` with the E2B adapter's layout (`stream.go`, conformance test). The
  store has no native events, so it uses the inert stream the simulator uses.

## Progress (2026-11-03)

- **Done:**
  - Step 1, the spike.
  - Step 2, ADR-0017.
  - Step 3, the `adapters/s3` module with `provision`.
  - The step 5 recipe, which passed live against SeaweedFS 4.51.
  - `just test-s3-adapter`, wired into `ci-fast` and CI.
  - The `dac-s3-adapter` image target.
  - `just ci-fast`, which passed.
- **Also done:** step 4, Compose wiring (`compose.artifacts.yaml`, `just test-artifact-store-stack` in CI).
- **Open:**
  - The end-to-end run through the control plane. It needs the Camer key, and the Codex qualification must
    read evidence from the store rather than the simulator.
  - A rerun from a committed revision.
  - The disposition, and `just ci`.

## Steps

1. **Spike.** A tagged Go test against a pinned SeaweedFS container. It probes each row of the table:
   - presigned PUT with signed `Content-Type`
   - versioned GET
   - `If-None-Match` on CopyObject
   - fence compare-and-swap with `If-Match`
   - Object Lock governance
   - lifecycle readback
   - the bucket-recreate trap

   Record the results. Stop and revisit the decision if a required behavior is missing.
   - verify: the spike output, committed to the evidence note.
2. **ADR-0017.** The store choice, the store-agnostic integrity rule, and freezing by version.
   - verify: review against ADR-0003 (no vendor fields in schemas).
3. **Adapter module.** Deterministic tests against a fake S3 plus the adapterkit conformance suite.
   - verify: `just test-s3-adapter`, `just test-conformance`.
4. **Wiring.** Replace the simulator artifact adapter in the Codex Compose profile with the S3 adapter
   plus SeaweedFS (pinned digest, bucket provisioning that is safe to re-run). Keep the simulator for
   deterministic suites.
   - verify: `just test-stack` on the simulator profile still passes.
5. **Qualification recipe** `just qualify-artifact-store`, against the real store. Each check maps to an
   acceptance criterion:
   - **Scope:** an upload with artifact A's capability does not reach B, another attempt, or another
     tenant.
   - **Expiry:** an expired capability is refused.
   - **Replay and failure injection:**
     - duplicate authorize, export and commit
     - lost commit reply
     - wrong bytes give `retry` and then succeed
     - a PUT after commit leaves the committed version unchanged
     - concurrent commits of two epochs produce one winner, the other `fenced`
   - **Restart:** kill the adapter between copy and reply. Restart it and replay. The receipt is
     identical.
   - **Retention and deletion:**
     - lifecycle on staging
     - locked final version refuses deletion without bypass
     - operator deletion, and what follows
   - verify: the recipe passes; add a CI workflow job if the store runs in CI.
6. **Evidence note** `docs/architecture/artifact-store-93.md`:
   - versions and digests, commands, durations, failures, limits
   - the final live, shadow or off decision
7. **Gate:** `just ci-fast`, `git diff --check`, then `just ci`.

## Out of scope

- An artifact browser UI.
- Semantic verification of results.
- Backup claims.
- Vendor fields in neutral schemas.
- Multipart upload: the runtime does one PUT.
- AWS S3 evidence, until an account is provided.
- Multiple adapter replicas racing between the fence read and the copy. The worst case is `fenced`,
  which the product already handles. It is documented as a limit.
