# Artifact byte store qualification — issue #93

Status:
- **Done:** the spike; the adapter ([`adapters/s3`](../../adapters/s3/README.md)); its live
  qualification against SeaweedFS, which passed.
- **Done since:** the Compose profile (`compose.artifacts.yaml`), deployed by `just test-artifact-store-stack`.
- **Also done:** the deployed lifecycle through the control plane, the S3 adapter and SeaweedFS, with the
  simulated runtime uploading over HTTPS.
- **Also done:** live Codex committing its result through the store.

Decision: [ADR-0017](../adr/0017-qualify-an-s3-api-artifact-store.md).

Disposition: **live** for the bundled SeaweedFS profile, within the limits recorded below. Every other store
stays off until its own qualification passes.

## Compose profile

### Live Codex through the store

Observed on 2026-11-03 at about 13:22 UTC with `just qualify-codex-artifact-store`, which finished in 253 seconds.
It used Codex app-server 0.165.0 against Camer's `deepseek-v4-flash-0731`, on the tree committed with this
paragraph. The environment role was simulated, in its own process.

`TestCodexRuntimeQualification` passed in 79.74 seconds:
- **Approval, restart and exact artifact (26.93 s).** Codex wrote its result after approval. The test read
  the bytes back through SeaweedFS's loopback filer, found exactly one version holding `qualified\n`, restarted
  the Codex adapter and the control plane, and the owner's acceptance reached `succeeded`.
- **Denial (52.81 s).** Two denied actions left no file in the workspace.

The control plane logged the `artifact_store` connection from the S3 adapter, and the key did not appear in the
Compose logs.

### Lifecycle through the store

Observed on 2026-11-03 at `52d8f5b` with `just test-artifact-store-stack`, which finished in 98 seconds. This run
layered the profile over the development stack.

The simulated runtime had no in-process store. Its scenario names an `artifact_export`, so it uploaded its
fixture with a plain HTTPS PUT to the presigned capability, carrying `Content-Type` and `Content-Length`
only. The control plane logged a single `artifact_store` connection, from `dac-s3-adapter`.

`TestDevelopmentStackLifecycle` then passed in 16.6 seconds:
- The task reached `ready_for_review` with one artifact and one integrity verification.
- After the owner accepted, it reached `succeeded`.

Read back through SeaweedFS's loopback filer, the objects bucket held exactly one version for the tenant,
and its bytes were the fixture's.

### Deployment over Codex and E2B

Observed earlier the same day, with the profile layered over the Codex and E2B overlays, in 63 seconds:
- The control plane logged `adapter connected` for the `artifact_store` role.
- `netstat` in the store's namespace listed one listener off loopback: `0.0.0.0:8443`.
- SeaweedFS was killed with `SIGKILL`. It came back healthy behind the same proxy, and `provision` passed
  again, including its conditional write.
- First provisioning took 10.4 seconds, most of it the lock warm-up.

Two things the deployment changed:
1. **Network exposure.** SeaweedFS serves plaintext S3, filer, master, volume and an unauthenticated IAM
   gRPC service. `-s3.port=0` binds a random port rather than disabling one. So every SeaweedFS listener
   is bound to loopback, and HAProxy 3.2.26 passes TLS through.
2. **Namespace ownership.** The proxy first shared the store's namespace. After a store restart it kept a
   dead namespace and connections were refused. Reversing the ownership fixed it.

## Live qualification

Observed on 2026-11-03 at 13:09:37 UTC with `just qualify-artifact-store`, on a clean tree at `52d8f5b`.

Versions:
- SeaweedFS `chrislusf/seaweedfs:4.51@sha256:f7a49e3f…134b8`; `weed version` reported
  `30GB 4.51 ffa87c25f linux arm64`.
- aws-sdk-go-v2 `1.47.5`, `service/s3` `1.114.6`, `smithy-go` `1.28.7`.
- Go `go1.26.10 darwin/arm64`, Docker `29.8.2`.
- Adapter: `52d8f5b`, with no uncommitted change under `adapters/s3`.

The run took 37 seconds, with the test itself taking 35.44 seconds. All 11 checks passed:

| Check | Observed | Time |
|---|---|---|
| `provision` twice in a row | Both succeeded and verified. Most of the time was the store's lock warm-up, described below, which `provision` now waits for. | 10.35 s |
| Adapter key preflight | Versioning and the staging expiry rule were read. | 0.00 s |
| Commit freezes the uploaded bytes | `missing_bytes`, then `digest_mismatch` after a wrong upload, then `committed` with the exact bytes readable by version. | 0.27 s |
| Replay and restart | The same receipt after a replay, from a new adapter, after a late upload through the same capability, and after an unconditional overwrite with the adapter key. The committed bytes did not change. | 0.05 s |
| Capability scope | 403 for another artifact, another attempt, another tenant, another content type, another length, and an expired capability. | 3.04 s |
| Stale writer | Epoch 1 was `fenced` before and after epoch 2 committed. | 0.06 s |
| Four concurrent replicas | One identical receipt each, and exactly one frozen version. | 1.04 s |
| Lost commit reply | The freeze was applied and the reply dropped. A new adapter replayed to `committed`. | 0.03 s |
| Adapter key deletes | Both a delete marker and a bypass delete of the locked version were refused. The receipt was unchanged. | 0.00 s |
| Store restart (`docker restart`) | The same receipt from a new adapter after the store came back. | 6.24 s |
| Operator deletion | Refused while the policy held. After the policy was removed, a bypass delete removed the version. A replay then failed with "committed object has no valid receipt" instead of inventing one, because an earlier overwrite remained. With a fresh artifact whose only version was deleted, a replay failed with "committed object was deleted" and no new version appeared. `provision` restored the policy. | 14.35 s |

Observed limits:
- **Conditional-write warm-up.** For about 9 seconds after the S3 listener first answered, and again
  after the restart, the store logged `no lock server found` for conditional writes. The writes waited
  and then succeeded, until the filer lock ring formed. Compose readiness must probe a conditional
  write.
- **Deletion breaks replay.** After an operator deletion, the commit's replay can no longer answer.
  Deletion is for retention and tenant removal only.
- **The deletion guard expires.** The commit marker expires with the staged bytes it protects, after
  `DAC_S3_STAGING_DAYS`. By then the staged bytes are gone too, so a replay cannot freeze them again.
- **Not observed:**
  - lifecycle expiry actually running
  - zero-byte artifacts
  - this live qualification on an amd64 host; on amd64, GitHub CI runs the deterministic adapter suite and
    the deployed stack

Failures on the way to this run:
- Review of the first revision found that a replay recreated a commit whose only version had been deleted.
  The earlier qualification missed it because an overwrite was still there. It also found that an
  authorization accepted changed arguments, that a fenced reply followed a late upload, and that startup
  accepted an expiry rule for another prefix. `52d8f5b` fixes all four.
- The first live run of that fix failed: the 32-byte read limit on fence objects refused the new digest and
  marker. The fake store had not enforced the limit. It does now, and the limit is 128 bytes.
- A local `just ci` lost `test-operations` to a host port clash with a concurrent qualification on port
  28443. The recipe then passed alone in 150 seconds. The port clash was in the run, not the code.

## Spike

Observed on 2026-11-03.

Store and client versions:
- SeaweedFS `chrislusf/seaweedfs:4.51`
  (`sha256:f7a49e3f3972ebed5be2ed4befa4a0c68eb7bdddba6e692d42729ceb545134b8`, linux/arm64, built
  2026-10-26), run as `weed mini`, with the S3 gateway on HTTPS from a test CA.
- aws-sdk-go-v2 `1.47.5`, `service/s3` `1.114.6`, `smithy-go` `1.28.7`.
- Go `go1.26.10 darwin/arm64`, Docker `29.8.2`.

How it ran:
- Two identities. The operator is an admin. The adapter has only `Read`, `Write` and `List` on the two
  buckets.
- Every presigned request went through a plain `net/http` client, not the SDK.
- The probe program lived in ignored scratch (`.work/spike-93`) and is not shipped. One full run took
  12.7 seconds.

| Probe | Observed |
|---|---|
| Objects bucket created with Object Lock and governance default retention of 1 day; staging bucket versioned, with a lifecycle rule | All accepted and read back. |
| S3 `CreateBucket` on the existing locked bucket | Refused with `BucketAlreadyExists`. The lock stayed. seaweedfs#11450 affects only `weed shell s3.bucket.create`, so provisioning must use the S3 API. |
| Presigned PUT, signed headers `content-type;host` | 200 with the signed type. 403 with another type, with the same signature on another artifact's key, on another tenant's key, and after expiry. |
| Presigned PUT with a wrong signed `x-amz-checksum-sha256` header | 400. Informational only: the design does not rely on it, and the runtime does not send it. |
| Replaying one capability | Creates a new staging version. GET by the first `VersionId` still returns the first bytes. |
| CopyObject of one pinned version to the objects bucket with `If-None-Match: *` | Copied the pinned bytes, kept the receipt metadata, returned a `VersionId`. A second conditional copy got `PreconditionFailed`. |
| Fence object created with `If-None-Match` and advanced with `If-Match` | Create twice was refused. A stale ETag was refused. |
| Default retention on the copy | Governance retention until the next day, read by the operator. The adapter gets `AccessDenied` reading retention, so it cannot inspect its own lock. |
| Adapter deleting the locked version, with or without the bypass header | `AccessDenied`. |
| Adapter `DeleteObject` without a version | Succeeded and left a delete marker. A later conditional copy then succeeded, because a marker counts as absent. |
| Adapter unconditional PUT to a frozen key | Succeeded and became the current version. The frozen version stayed readable by `VersionId`. |
| Bucket policy denying `s3:DeleteObject` and `s3:DeleteObjectVersion` to everyone | Adapter delete markers were refused. Operator deletion was refused too, so deletion is done by removing the policy first. |
| Bucket policy denying `s3:PutObject` without `s3:if-none-match` | Refused the conditional copy as well (AWS answers 501 there), so it cannot be used. |
| Operator delete of a locked version with the bypass header (before the policy) | Succeeded. A later GET of that version got `NoSuchVersion`. |
| Version order | `LastModified` has one-second resolution, so all four versions tied. `ListObjectVersions` returned them newest first, which is the S3-documented order. SeaweedFS version ids also sort descending, but AWS ids are opaque. |
| Store restart (`docker restart`) | Ready in 6 seconds. Lock configuration, versions, receipt metadata, bucket policy and lifecycle rule were all intact. |
| Presigned GET through a plain client (aws-sdk-go-v2#3528) | 200 on 1.47.5, with `ResponseChecksumValidation: WhenRequired`. |

## What the spike changed in the design

1. **The winner is the first stored version, not the current one.**
   - The adapter key can overwrite the current version.
   - Commit and replay find the winner as the oldest version of the final key in `ListObjectVersions`
     order.
   - Every read uses an explicit `VersionId`.
   - Neither `LastModified` nor the shape of a version id is used. That keeps the rule valid on AWS.
2. **A bucket policy keeps delete markers out.** It denies object and version deletes to everyone. That
   closes the marker path that would let a second conditional copy succeed. Operator deletion becomes two
   steps: remove the policy, then delete with governance bypass. The qualification records this as the
   documented deletion procedure.
3. **The adapter key is not an admin.** As expected, it cannot bypass governance. The startup check reads
   the lock configuration with the adapter key, and that read was not denied.

## Left open by the spike

The live qualification above has since observed concurrent commits and a lost commit reply. That reply
loss was injected after a real freeze, not by killing the adapter process; the adapter keeps no local
state, so a kill loses nothing more.

Still open:
- **Lifecycle expiry running.** It is day-granular and was not waited for.

The deployed stack has since exercised upload links signed for the `artifact-store` Compose service name.
