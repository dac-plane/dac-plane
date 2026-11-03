# Issue #95 — Publish the first supported dac-plane distribution

Tracker: https://github.com/dac-plane/dac-plane/issues/95
Branch: `chore/95-first-release`, from `main` at `a4a2798`.
Parent: #94. Blocks #96 and #97.

## Where the tree stands

Surveyed on 2026-11-03. Each item blocks a release or changes its design.

| Finding | Evidence | Consequence |
|---|---|---|
| No license file | `git ls-files` finds no license or notice file; the repository is public | Nobody may lawfully run or build on a release. The Codex image also redistributes `codex-app-server`, and SPEC section "Licensing / upgrades" asks to retain notices and review terms. |
| Binaries and images carry no version | No `version` command, `-X` flag or OCI label; `.git` is excluded from the build context | The tag, labels and digest cannot be tied together, which the first acceptance item requires. |
| The kit's published `go.mod` would pin a flagged dependency | `adapterkit` and `adapters/codex` require `golang.org/x/text v0.14.0`; Trivy reports CVE-2026-56851 and CVE-2026-56852 as HIGH in the Codex binary | Fix before the kit is published, because a published `go.mod` sets the floor for every consumer. `govulncheck` passes on this tree, so it does not catch this. |
| Two adapter images fail any sensible vulnerability gate | `bookworm-slim`: 1 CRITICAL, 32 HIGH, 1 fixable, on both Codex and E2B. Distroless `dac-plane` and S3 images: 4 UNKNOWN, all fixable | See decision 4. Both adapters are static and neither calls a shell. |
| `.env` is not in `.dockerignore` | The root `.env` holds the E2B key; `Dockerfile` copies named paths only, so it reaches the local build context and not an image | Exclude it. Add a secret scan of the images as the gate. |
| Tests need a shell in the Codex adapter image | `qualification_test.go:298` runs `docker exec ... test` in that container | Moving it to distroless means changing those helpers. |
| Qualification rebuilds its images | Overlays have `build:` sections and `:qualification` default tags | #75's disposition covers local builds. A digest needs a no-build mode in `qualify-providers`. |
| Amd64 was never live-qualified | The #75 runs used arm64 adapters; the amd64 Codex archive is pinned but its image was never run live | Publish it only with the limit stated, or qualify it. See decision 3. |
| Nothing protects tags or `main` | No rulesets; `main` is not protected | A signed tag means little if it can move. The exploration rejected AgentOven partly because "the same release tag was moved". |
| The kit has no README | `adapterkit/` has only code | An external author, and #97, has nothing to start from. |

Ready already: `image-repro` gives identical digests, `MinServeVersion` 13 and `CurrentVersion` 15 exist, OpenAPI is
at 1.0.0, ControlFrame is protocol 1, ADR-0011 sets the upgrade order, and a signing key is registered on GitHub.
Confirmed by test: an adapter's `go.mod` can require an unpublished kit version while its `replace` stays, and
`tidy`, `build` and `verify` all pass.

## Decisions

Decided by the project owner on 2026-11-03: Apache-2.0, and `v0.1.0` for both tags.

1. **License.** Apache-2.0 for the whole repository.
2. **Version numbers.** Control plane `v0.1.0` and kit `adapterkit/v0.1.0`, on the same commit. ADR-0008 says a kit
   major matches a protocol major, which implies kit v1 today. But v1 freezes the kit's API before #97, its first
   external consumer, has used it, and a break would then need a `/v2` module path. So ADR-0018 records that v0 speaks
   protocol 1, and that kit v1.0.0 follows #97.
3. **Architectures.** Publish `linux/amd64` and `linux/arm64`, built natively on GitHub's runners, and state that only
   arm64 is live-qualified. #96 must qualify amd64 if the pilot runs on it.
4. **Adapter base image.** Move Codex and E2B to distroless static, like S3. This removes the OS findings instead of
   allowlisting 33 of them. The fallback, if `codex-app-server` needs the OS, is to keep Debian on Codex only and record
   each finding.
5. **Vulnerability gate.** Fail on any fixable HIGH or CRITICAL finding in an image or binary, and on any `govulncheck`
   finding. Publish everything else as evidence. An unfixable HIGH or CRITICAL finding needs a dated entry, with its
   reason, in a reviewed file.
6. **Replace directives.** Keep them in this repository, and require the real kit version in each `go.mod`. This
   replaces my earlier note that they must go. Removing them would need two commits per release and would stop CI
   testing adapters against the current kit. Images built from the tag contain exactly the tagged kit, and step 5
   proves the published kit builds every in-tree adapter.
7. **Registry.** `ghcr.io/dac-plane/{dac-plane,dac-codex-adapter,dac-e2b-adapter,dac-s3-adapter}`, public. The
   simulator and `operationstest` targets are not published.

Tags never move and never get deleted. A failed release gets the next patch number. The Go module proxy caches a
version permanently, so all rehearsal uses `-rc.N` tags.

## Steps

1. **Record the release contract.** License and third-party notices. ADR-0018: versioning, the compatibility set, the
   gate, and tags never moving. Kit README.
   Verify: notices cover every Go dependency and `codex-app-server`.
2. **Make the tree releasable.** Bump `x/text` in the kit and Codex adapter. Refresh the distroless digest. Extend
   `.dockerignore`. Add `dac-plane version`, printing version, revision, schema range and protocol. Add `VERSION` and
   `REVISION` build arguments and OCI labels to all four targets. Move Codex and E2B to distroless and replace the
   shell-based test helpers.
   Verify: `just ci-fast`, `just image-repro`, Trivy shows no fixable HIGH or CRITICAL, and `just qualify-providers`
   passes on the new bases.
3. **Build per architecture.** A `release-image` recipe builds one target for one platform with fixed timestamps and
   no inline attestations, since those would change the digest. `image-repro` keeps checking that two builds match.
   Verify: identical digests on amd64 and arm64 from one tree.
4. **Write the release workflow.** `.github/workflows/release.yml`, on `v*` tags, with a protected `release`
   environment. `ci.yml` and `security.yml` gain `workflow_call`.
   Order: verify the tag, full CI, build each architecture twice and compare, publish by digest, join into one index,
   scan, attest provenance and SBOM, clean-room, and only then the GitHub Release.
   Tag check: both tags exist on one commit, GitHub reports both signatures verified, and every `go.mod` requires that
   kit version.
   Least privilege per job, actions pinned by SHA, scanners installed by checksum. I will check current action versions
   upstream at this point, because Context7 is not connected in this session.
   Verify: `just lint-workflows`.
5. **Add the clean room.** `just verify-release <tag>`, from an empty directory with no Docker credentials, and
   `GOWORK=off`:
   - Pull all four images by digest and check labels against the tag.
   - Run `dac-plane version`, then `migrate` and `serve` on the base Compose stack until `health ready` passes.
   - Check `gh attestation verify` for each image.
   - Compile `adapterkit/examples/minimal` against the published kit, and run `adapter-conformance` at that version.
   - Drop the `replace` from a copy of each in-tree adapter and build it against the published kit.
6. **Rehearse, including failures.** Push `v0.1.0-rc.1` and `adapterkit/v0.1.0-rc.1` to `ghcr.io/dac-plane/dry-run/*`.
   Prove the gate stops on an unsigned tag, a mismatched kit version, a planted vulnerable dependency and a planted
   fake secret, and that nothing reaches the release namespace in those runs.
   Then run the full `just qualify-providers` against the rc digests, with no build.
7. **Write the notes.** `docs/releases/v0.1.0.md`: the compatibility table (image digests, kit, schema 13 to 15,
   Surface API v1, protocol 1, Codex 0.165.0, E2B template source, SeaweedFS 4.51, PostgreSQL 16), upgrade order and
   rollback limits from ADR-0011, known limits from the #75 note, and how to report a vulnerability. No availability or
   support claim. Assets: OpenAPI, JSON Schemas, base Compose stack, SBOMs, scan results, and a digest manifest for #96.
8. **Release.** Add tag rulesets, enable immutable releases, and have the project owner sign and push `v0.1.0` and
   `adapterkit/v0.1.0`. Rerun the clean room against the published release. Record the evidence in
   `docs/architecture/release-95.md`, including failed runs. Tick the #95 acceptance items and link the digests from
   #96 and #91's promotion rule.

## Layout checked against established projects

Compared on 2026-11-03 with containerd, Prometheus, cert-manager, dex, cosign, the OpenTelemetry Collector, Tempo,
Temporal and Keycloak, and with OpenSSF Scorecard's checks. Scripts moved to `scripts/release`, the external adapter
became a kit example, and `SECURITY.md`, `RELEASES.md`, `CODEOWNERS`, Dependabot and Scorecard were added. Following
Keycloak, `RELEASES.md` states policy for users, and the procedure and note template live in `docs/releases`.

## Owner-only actions

Choosing the license, signing and pushing the final tags, approving the `release` environment, making the packages
public, and turning on private vulnerability reporting and Dependabot alerts. The environments, tag ruleset and
immutable releases were applied on 2026-11-03.

## Out of scope

Compose overlays for real providers, an OIDC issuer, backup and operating the pilot (#96). The reference application
(#97). Kubernetes. Protecting `main`, which is worth doing but is not needed for a tag gate. Moving the rolling-upgrade
fixture from `ca20b3e` to a published digest, which should follow the first release.

## Limits to carry into the release notes

Live evidence is one full run on one arm64 host, from before the rebuilt images, until step 6 repeats it. A provision
can still stall as the #75 note records. Reproducibility is proved on GitHub's runners for this Dockerfile only, and
E2B Cloud's own build cannot be pinned.
