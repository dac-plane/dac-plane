# ADR-0017: Store artifact bytes through one S3-API adapter, SeaweedFS first

- Status: Accepted
- Date: 2026-11-03
- Issues: [#93](https://github.com/dac-plane/dac-plane/issues/93)

## Context

[#26](../architecture/artifact-results-26.md) fixed the staged artifact protocol:
- `authorize_upload` returns an opaque upload capability held only in memory.
- The runtime uploads the bytes directly to the store.
- `commit_manifest` must hash and freeze one object atomically, replay exactly, and answer `committed`,
  `retry` or `fenced` (ADR-0003).

Until now, only the simulator stood behind that protocol. The pre-implementation review left the byte
store open.

The candidates compared were AWS S3, SeaweedFS, Ceph RGW, Garage, MinIO, Cloudflare R2 and RustFS. The
comparison is in [`tasks/plan.md`](../../tasks/plan.md) at the time of this decision, and the spike
observations are in [artifact-store-93.md](../architecture/artifact-store-93.md):
- Garage was rejected because it cannot freeze a version (no versioning, no Object Lock).
- MinIO CE was rejected because it is in maintenance mode.
- R2 was rejected because it lacks S3 Object Lock and lifecycle.
- RustFS was not chosen because of how young it is and its recent version-overwrite advisory.
- Ceph RGW is capable, but too heavy to run for a bounded qualification.

The operator provides the store, as they provide PostgreSQL (ADR-0009). The pilot runs the reference
Compose stack.

## Decision

**One adapter, speaking the S3 API.** `dac-s3-adapter` is an out-of-process adapter on `adapterkit`. It
uses only:
- presigned PUT and GET
- HEAD
- versioned GET
- `ListObjectVersions`
- CopyObject with `x-amz-copy-source-version-id` and `If-None-Match`
- PutObject with `If-None-Match` and `If-Match`
- bucket Object Lock, lifecycle and policy configuration

No vendor name or field reaches a neutral schema.

**SeaweedFS is qualified first.** It runs in the reference stack, pinned by digest. AWS S3 is the next
target. It takes a configuration change and a new evidence run, not new code. Any other store is
unsupported until its own qualification passes.

**The adapter hashes the bytes itself.**
- The store's checksums are not the integrity gate.
  - AWS checks a checksum only when it arrives as a header.
  - The runtime sends none.
  - SeaweedFS has ignored the checksum algorithm of presigned uploads in the past.
- Commit reads one pinned staging version, hashes it within the manifest's size bound, and freezes that
  same version.

**Freeze by version, and the first stored version wins.**
- Commit copies exactly the hashed staging version into a separate objects bucket with
  `If-None-Match: *`. The copy carries the receipt in its metadata.
- The committed object is the oldest version of its key in `ListObjectVersions` order, always read by
  `VersionId`.
- An overwrite of the current version therefore cannot change a recorded outcome. The spike showed that
  the adapter's own key can make such an overwrite.

**Two buckets, so that locks and expiry do not collide.**
- Staging is versioned and expires by lifecycle. It holds uploads and the per-artifact writer fence.
- The objects bucket has Object Lock with governance-mode default retention.
- A bucket policy on the objects bucket denies object and version deletes to everyone. This keeps out the
  delete marker that would let a second conditional copy succeed.
- Deleting a committed object is an operator procedure, done with a credential the adapter never holds:
  remove the policy, then delete with governance bypass.

**No local state.** The fence, the authorization digest and the commit receipt all live in the store.
After a restart, the adapter returns the same outcome from the same store. It offers `operation.replay`
(ADR-0012) and serializes deliveries per operation.

**Configuration is applied once and checked at every start.**
- `dac-s3-adapter provision` runs with the operator's key as a one-shot step, like `dac-plane migrate`.
  - It creates or completes both buckets through the S3 API, never through `weed shell
    s3.bucket.create`. That command replaces an existing bucket's lock and lifecycle settings
    (seaweedfs#11450).
  - It applies versioning, retention, lifecycle and the delete-denying policy, and reads each one back.
- On every start, the adapter checks with its own key that both buckets are versioned and that staging
  expires. If either is missing, it refuses to serve.
  - It cannot check Object Lock itself: SeaweedFS denies a non-admin key the lock configuration read, and
    the adapter key must not be an admin.

**Uploads have an exact length.** The presigned PUT signs `Content-Length` together with the media type,
so a staged upload always has the manifest's size, and hashing it is bounded by that size.

## Consequences

- **Upload capabilities are presigned URLs.** Each one is bound to one staging key, the PUT method, the
  manifest's media type and an expiry. Its host must be the endpoint the runtime reaches, over HTTPS.
- **A capability can be replayed until it expires.** Each replay creates a new staging version. Commit
  pins one version, so a later upload cannot change committed bytes. It only costs staging space until
  lifecycle expiry.
- **Multiple adapter replicas can race** between reading the fence and copying the object. The worst
  outcome is `fenced` for the newer writer. The product already handles that, and it is documented as a
  limit.
- **Operator deletion of a committed object is destructive.** A later replay of that commit fails instead
  of returning a receipt. The product already holds the admitted metadata, so deletion is for retention
  and tenant removal, not for repair.
- **SeaweedFS needs a warm-up after start.** Its conditional writes wait for the filer lock ring, which
  formed about 9 seconds after the S3 listener answered. Store readiness is therefore a successful
  conditional write, not an HTTP answer.
- **Deletion and retention behave as the qualification observes them.** No backup claim is made.
- **The store and adapter versions stay pinned exactly.** A SeaweedFS or SDK upgrade repeats
  `just qualify-artifact-store`.
