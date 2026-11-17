# Let a surface read and download a result's artifacts

Tracker: https://github.com/dac-plane/dac-plane/issues/153
Branch: `feat/153-artifact-download`, from `main` at `8f71cc4`.
Decision: `docs/adr/0025-serve-artifacts-to-surfaces-through-the-control-plane.md`.
Needed by: the reference application (#97), whose reviewer must read the review before accepting it.

## Where the tree stands

| Finding | Evidence | Consequence |
|---|---|---|
| The Surface API lists artifact identifiers only | `result-revision.schema.json`, `docs/guides/use/surface-api.md` "Artifacts" | A record read and a content read are added. |
| The admitted record already has what a surface shows | `lifecycle/artifact.schema.json`: kind, media type, size, digest, attempt, commit time | The record read serves it as it is. |
| The verification holds the committed object version | `GetArtifactVerification`, `artifact-verification.schema.json` | The download names that exact version. |
| The artifact store role has no read | `contracts.Methods(RoleArtifactStore)` | A new method, behind a negotiated capability. |
| A result is bounded to 64 KiB | `adapterkit/wire/codec.go` | Bytes cannot travel in replies; the store presigns a read. |
| The store is unpublished | `docs/guides/deploy/index.md` | The control plane fetches and relays; surfaces never reach the store. |
| A connection's requests are sent directly, outside operations | `application.connectionRequest` | The download request follows that path. |

## Stages

1. **Decision and contracts.** ADR-0025; `authorize_download`, `artifact.download`, `DownloadRequest` and
   `DownloadAuthorized` with JSON Schemas, frame enum, agreement tests and the kit mirror.
2. **Adapters.** The S3 adapter presigns a versioned GET of the objects bucket and offers the capability; the
   simulator's byte store and the simulator adapter's HTTPS fixture serve downloads; conformance scenarios.
3. **Application and Surface API.** Owner-scoped record read; the content read with origin checks, length and
   digest checks, safe headers; `serve` settings; OpenAPI and problems.
4. **Deployment and documentation.** Compose wiring for the reference and development stacks; the Surface API
   guide, configuration reference and adapter pages; an end-to-end check in the artifact store stack.

## Decisions

Recorded as each stage settles them.
