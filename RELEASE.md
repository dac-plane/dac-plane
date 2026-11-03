# Releases

[ADR-0018](docs/adr/0018-release-versioning-and-gate.md) records why releases work this way.

## Versioning

- The control plane uses semantic versions, tagged `vX.Y.Z`.
- The adapter kit is versioned separately, tagged `adapterkit/vX.Y.Z`. While it is v0, a minor version may change its
  Go API. Kit v1.0.0 follows the first external consumer.
- Each release's notes in [docs/releases](docs/releases) name the kit, schema range, API and protocol versions it
  works with. Upgrades follow [ADR-0011](docs/adr/0011-roll-compatible-binaries-before-schema-migrations.md).
- Tags never move and are never deleted. A failed release gets the next patch number.

## Cutting a release

1. Merge a change to `main` with the release notes, a [CHANGELOG](CHANGELOG.md) entry, and the kit version in every
   `go.mod`.
2. Rehearse with signed `-rc.N` tags. They publish to `ghcr.io/dac-plane/dry-run` and draft a prerelease.

   ```sh
   git tag -s v0.1.0-rc.1 -m "dac-plane v0.1.0-rc.1"
   git tag -s adapterkit/v0.1.0-rc.1 -m "adapterkit v0.1.0-rc.1"
   git push origin v0.1.0-rc.1 adapterkit/v0.1.0-rc.1
   ```

3. Run live qualification against the published digests:
   `DAC_PROVIDER_PUBLISHED_IMAGES=1 DAC_PLANE_IMAGE=... DAC_CODEX_ADAPTER_IMAGE=... DAC_E2B_ADAPTER_IMAGE=... DAC_S3_ADAPTER_IMAGE=... just qualify-providers`.
4. Push the same tags without the suffix, from `main`. Approve the `release` environment when asked.
5. On a new package, make it public. Then publish the draft and repeat the clean room with no registry login:

   ```sh
   gh release download v0.1.0 -p 'release-manifest.json' -p 'dac-plane-compose-*' -D .work/verify
   DAC_RELEASE_ANONYMOUS=1 scripts/release/verify-release.sh .work/verify/release-manifest.json
   ```

## Verifying a release

```sh
gh attestation verify oci://ghcr.io/dac-plane/dac-plane@sha256:DIGEST --repo dac-plane/dac-plane
```
