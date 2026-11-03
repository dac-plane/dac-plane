# ADR-0018: Version a release as one compatibility set, behind one gate

- Status: Accepted
- Date: 2026-11-03
- Issue: [#95](https://github.com/dac-plane/dac-plane/issues/95)

## Context

[ADR-0009](0009-deployment-unit-and-first-substrate.md) ships one control-plane image and separate adapter
images, and [ADR-0008](0008-adapter-process-boundary-and-wire-transport.md) versions the kit separately. Nothing
is published yet, nothing in a binary or image names a version, and the repository has no license.

ADR-0008 says a kit major version corresponds to a ControlFrame protocol major version. Read strictly, the first
kit is v1, because the protocol is 1. A Go v1 freezes the kit's exported API, and a break would then need a `/v2`
module path. The kit's only consumers so far are the adapters in this repository;
[#97](https://github.com/dac-plane/dac-plane/issues/97) is its first external user.

The exploration rejected one candidate partly because
[the same release tag was moved](../../architecture-explore/docs/qualification/control-plane-candidates.md) while
release fixes were attempted. The Go module proxy also keeps a published version permanently.

## Decision

**License.** The repository is Apache-2.0. Every published image carries the license and third-party notices under
`/licenses`. `just notices-check` fails if the notices do not match the modules linked into the released binaries.

**Versions.** The first release is control plane `v0.1.0` with kit `adapterkit/v0.1.0`. This narrows ADR-0008:
while the kit is v0 it speaks protocol 1 and may change its Go API in a minor version. Kit v1.0.0 follows #97, and
from then on ADR-0008's rule applies as written. The Surface API keeps its own version, 1.0.0 in the OpenAPI
document, and a control plane keeps supporting every protocol version it advertises.

**A compatibility set.** A release names one control-plane version, one revision, one kit version, the serving
schema range, the Surface API and protocol versions, the pinned provider versions, and the digest of each of
`dac-plane`, `dac-codex-adapter`, `dac-e2b-adapter` and `dac-s3-adapter`. The simulator and the
`dac-codex-adapter-local` target are not published. The local target exists for recipes that run Codex commands in
the adapter container; the release supports commands in the sandbox only (ADR-0016).

**Replace directives stay.** Modules in this repository keep `replace` lines for the kit, and require the kit
version they ship with, so a kit change and its adapters land in one commit and CI tests them together. The gate
checks that the tree's `adapterkit/` matches the kit tag exactly, and builds every in-tree module against the
published kit with the `replace` removed.

**Tags.** A release is a pair of signed annotated tags, `vX.Y.Z` and `adapterkit/vX.Y.Z`. They never move and are
never deleted. A failed release gets the next patch number. Rehearsals use `-rc.N` tags and publish under
`ghcr.io/dac-plane/dry-run`. A final tag must be on `main`.

**The gate.** `.github/workflows/release.yml` rebuilds from the tag and stops on the first failure, in this order:

1. Both tags are signed, verified by GitHub and consistent with the tree.
2. The full CI and security workflows pass.
3. Each image for each architecture builds twice, the second time without cache, with identical digests.
4. No fixable HIGH or CRITICAL finding and no secret appears in any image. `just vuln` covers reachable Go
   vulnerabilities in step 2.
5. Images are pushed by digest, attested with provenance and an SBOM, and joined into one index.
6. The clean-room check passes against what was published.
7. A draft GitHub Release is created. A person publishes it.

An unfixable HIGH or CRITICAL finding needs an entry in `.trivyignore.yaml` with its reason and an expiry date, so
an exception is reviewed and lapses.

Inline BuildKit attestations are not used, because they would change the digest that step 3 compares. Attestations
are stored separately and verified by `gh attestation verify`.

Scanners are installed as checksum-verified release binaries, not as GitHub Actions. The Trivy project reported a
[supply-chain compromise of its ecosystem](https://github.com/aquasecurity/trivy/security/advisories) in March 2026.

## Alternatives considered

- **Kit v1.0.0 now:** rejected. It freezes the API before its first external user has tried it.
- **Removing `replace` and tagging the kit first:** rejected. Every kit change would need two commits and a
  publication before adapters could use it, and CI would stop testing adapters against the current kit.
- **BuildKit `--provenance` and `--sbom`:** rejected, as above.
- **Allowlisting the findings in `bookworm-slim`:** rejected for the Codex and E2B images. Both programs are static,
  so distroless removes the findings.
- **`cosign` signatures in addition to attestations:** not adopted. GitHub attestations are Sigstore-signed and tie
  an image to this repository's workflow. Nothing yet requires a second mechanism.

- **GoReleaser:** not adopted. It mainly builds and packages binaries, and this release publishes images only.

## Consequences

- A release needs a person to sign tags and publish the draft. Nothing publishes on merge.
- The first push of a package is private, so an owner makes it public before the anonymous clean-room check.
- Amd64 images are published with only arm64 live-qualified until a pilot on amd64 qualifies them.
- `codex-app-server` ships with OpenAI's license and notice. Its release archive carries no notices for the Rust
  crates compiled into it, so those are not reproduced. This needs review if redistribution terms become a concern.
- Dropping a schema from the serving range, or a protocol version, is a release-note item under ADR-0011.
