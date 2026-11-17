# ADR-0025: Serve artifacts to surfaces through the control plane

- Status: Accepted
- Date: 2026-11-17
- Issue: [#153](https://github.com/dac-plane/dac-plane/issues/153)
- Refines: [ADR-0017](0017-qualify-an-s3-api-artifact-store.md)

## Context

A result revision lists its artifacts by identifier only, and the Surface API serves neither the admitted artifact
record nor its bytes. A surface cannot show what an agent produced: the reviewer of a written review cannot read the
review before accepting it. The reference application (#97) needs both.

What exists:

- The admitted artifact record holds the kind, media type, size, SHA-256 digest, producer, attempt and commit time;
  its verification records the store and the committed object version (ADR-0006, ADR-0017).
- The artifact store role has `authorize_upload`, `stat_object` and `commit_manifest`. There is no read.
- The S3 adapter presigns uploads for the runtime and keeps committed objects under Object Lock, read by version.
- The reference deployment publishes only the Surface API and the issuer. The artifact store, the adapter endpoint
  and PostgreSQL stay unpublished, and the deploy guide says so.
- A request or reply result is bounded to 64 KiB, and a frame to 1 MiB.

## Decision

**The Surface API reads an artifact's record and streams its bytes.**
`GET /v1/tenants/{tenant}/tasks/{task}/artifacts/{artifact}` returns the admitted record of an artifact of that task.
`GET .../artifacts/{artifact}/content` returns its bytes. Both are owner-scoped like the task: a principal that cannot
read the task, or an artifact of another task, gets 404.

**The artifact store presigns a read of the exact committed version, and the control plane fetches it.** A new
artifact store method, `authorize_download`, takes the artifact's manifest and committed object version and returns
a URL that reads exactly that version until it expires, at most fifteen minutes. The request names the artifact's
attempt, as every artifact store request does, and is sent directly, like a connection's requests: it is read-only and
idempotent, and never becomes a provider operation. Only an adapter that selected the new capability
`artifact.download` is sent it; an older adapter keeps working and its artifacts answer `503 artifact-unavailable`.

**The control plane relays the bytes and checks them.** It fetches the URL only when its origin is one the operator
lists in `DAC_ARTIFACT_DOWNLOAD_ORIGINS`, trusting the roots in `DAC_ARTIFACT_DOWNLOAD_CA_FILE` when set, and follows
no redirect. The response carries the admitted media type, length and digest (`Repr-Digest`, RFC 9530) and is sent
as an attachment with `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`, because the bytes
are agent output. A store answer whose length differs from the record is refused before any byte is sent; bytes
whose digest differs abort the response, so no complete body with the wrong digest is ever delivered. A store that sends
nothing for thirty seconds ends the download the same way, since the Surface API has no write timeout of its own.

## Alternatives considered

- **Redirect the surface to the presigned URL:** rejected for the reference deployment. The surface would need to
  reach the store, which is deliberately unpublished, and the redirect would hand a bearer capability to the browser.
  A deployment that publishes its store could add it later as an option without changing this contract.
- **Carry the bytes in adapter replies:** rejected. A result is bounded to 64 KiB, so a megabyte would take dozens of
  round trips through the control protocol, which exists for commands and evidence, not bulk data.
- **Let the control plane read the store with its own credentials:** rejected. ADR-0017 keeps the S3 API behind one
  adapter, and the control plane would then hold store credentials and depend on one store's API.
- **Follow any URL the adapter returns:** rejected. A compromised adapter could make the control plane fetch an
  internal address and relay it to a tenant; the operator's origin list bounds that.

## Consequences

- Downloads cost control-plane bandwidth. The pilot's artifacts are reports and diffs, and the stream is never
  buffered whole.
- `serve` gains two settings. Without `DAC_ARTIFACT_DOWNLOAD_ORIGINS`, records are served and downloads answer
  `503 artifact-unavailable`.
- The kit gains `authorize_download` and `artifact.download`; adapters built with v0.3.0 keep working.
- The S3 adapter's presigned reads are signed for its public endpoint, which the control plane must reach.
