# S3 artifact store adapter

`dac-s3-adapter` serves the `artifact_store` role over the S3 API. It is built on `adapterkit` and does
not import DAC internals. [ADR-0017](../../docs/adr/0017-qualify-an-s3-api-artifact-store.md) records the
decision. [artifact-store-93.md](../../docs/architecture/artifact-store-93.md) records what has been
observed.

Qualified store: SeaweedFS `4.51`, pinned by digest in [`qualification/run.sh`](qualification/run.sh).
"S3-compatible" alone is not a support claim. Another store is supported only once
`just qualify-artifact-store` passes against it at a recorded version.

## How bytes move

1. **`authorize_upload`**
   - Raises the artifact's writer fence to the manifest's epoch. The fence is a compare-and-swap on
     `fences/{tenant}/{artifact}`.
   - Returns a presigned PUT for `staging/{tenant}/{attempt}/{artifact}/{epoch}`. The signature covers
     the host, the manifest's media type and its exact length. It expires after `DAC_S3_UPLOAD_TTL`.
   - A replay presigns the same key again. The same operation with other arguments is rejected, compared
     through a digest at `authorizations/{tenant}/{operation}`.
2. **The runtime uploads directly to the staging bucket.** Bytes never pass through the control plane or
   this adapter.
3. **`stat_object`** hashes the current staging version. It is diagnostic only.
4. **`commit_manifest`**
   - Hashes one pinned staging version itself. Store checksums are not trusted.
   - Answers `retry` when the bytes are missing or differ.
   - Otherwise copies exactly that version into the objects bucket with `If-None-Match: *`. The copy's
     metadata carries the receipt.
   - The committed object is the oldest version of `{tenant}/{artifact}`. Replays, and restarts of the
     adapter or the store, rebuild the same receipt from it.
   - A marker at `commits/{tenant}/{artifact}` stops a replay from recreating a deleted commit while its
     staged bytes remain. It expires with them.
   - A different manifest or a stale epoch is `fenced`. The reply carries no observation, so a late upload
     cannot change it.

The adapter keeps no local state and offers `operation.replay`.

## Buckets and keys

| Bucket | Configuration | Holds |
|---|---|---|
| staging | versioning; lifecycle expiry of current and noncurrent versions after `DAC_S3_STAGING_DAYS` | uploads and fences |
| objects | Object Lock with governance default retention of `DAC_S3_RETENTION_DAYS`; a policy denying object and version deletes to everyone | committed artifacts |

`dac-s3-adapter provision` creates and configures both buckets through the S3 API with the operator's
key. It then reads everything back.
- It is safe to run again.
- Never create or re-create these buckets with `weed shell s3.bucket.create`. That command replaces an
  existing bucket's lock and lifecycle settings (seaweedfs#11450).

The adapter's own key:
- **Allowed:** read, write and list on both buckets.
- **Forbidden:** an admin identity. SeaweedFS admins bypass governance retention.
- **At startup:** the adapter checks versioning and the staging expiry rule, and refuses to serve if
  either is missing.

**Deleting a committed artifact** takes the operator key and two steps:
1. Remove the objects bucket policy.
2. Delete the version with `x-amz-bypass-governance-retention: true`.

Then run `provision` again to restore the policy. A deleted commit can no longer be replayed: the adapter
reports an error rather than inventing a receipt.

## Configuration

| Setting | Purpose |
|---|---|
| `DAC_S3_ENDPOINT` | HTTPS origin where the adapter reaches the store |
| `DAC_S3_PUBLIC_ENDPOINT` | HTTPS origin where the runtime reaches it. Presigned URLs are signed for this host. Defaults to `DAC_S3_ENDPOINT` |
| `DAC_S3_REGION` | Signing region; defaults to `us-east-1` |
| `DAC_S3_ACCESS_KEY_ID`, `DAC_S3_SECRET_ACCESS_KEY_FILE` | The adapter key, or the operator key for `provision` |
| `DAC_S3_STAGING_BUCKET`, `DAC_S3_OBJECTS_BUCKET` | Two distinct buckets |
| `DAC_S3_CA_FILE` | CA for the store's certificate; system roots when unset |
| `DAC_S3_UPLOAD_TTL` | Capability lifetime as a Go duration; defaults to `15m` |
| `DAC_S3_RETENTION_DAYS`, `DAC_S3_STAGING_DAYS` | `provision` only; default `30` and `2`. Staging must outlast the 24-hour artifact deadline plus the capability lifetime |
| `DAC_PROVIDER_ID`, `DAC_ADAPTER_URL`, `DAC_ADAPTER_CA_FILE` | Control-plane registration and endpoint |
| `DAC_ADAPTER_TOKEN_FILE` or the `DAC_ADAPTER_*` client-credentials settings | Adapter authentication |

## Verification

- **`just test-s3-adapter`** runs:
  - deterministic tests against an in-memory store with S3 version semantics
  - the published ControlFrame conformance suite
  - schema checks of every reply against `contracts/jsonschema/adapter`
- **`just qualify-artifact-store`** starts a throwaway SeaweedFS, pinned by digest, with a test CA and
  per-run keys. It runs the live checks against it through the adapter's own code, and writes versions,
  durations and output under `.work/s3-qualification/`. It needs Docker and no credential.
