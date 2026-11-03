# Releases

[ADR-0018](docs/adr/0018-release-versioning-and-gate.md) records why releases work this way. Maintainers cut a release
by following [docs/releases](docs/releases/README.md).

## Versioning

The control plane uses semantic versions, tagged `vX.Y.Z`.

The adapter kit is versioned separately, tagged `adapterkit/vX.Y.Z`. While it is v0, a minor version may change its Go
API. Kit v1.0.0 follows its first external consumer. A release may reuse an older kit tag if the kit has not changed.

Each release's notes name the kit, schema range, API and protocol versions it works with.

## Release cadence

No cadence has been established. Releases are made as needed.

## Backwards compatibility

Before 1.0.0, a minor version may include breaking changes. The release notes list them under Upgrading. Upgrades
follow [ADR-0011](docs/adr/0011-roll-compatible-binaries-before-schema-migrations.md): replace every binary first, then
migrate the schema.

## Security releases

Fixes ship as a patch to the latest release, and its notes describe them. See [SECURITY.md](SECURITY.md) to report a
vulnerability.

## Branches and tags

Development happens on `main`, and final releases are tagged from it. Rehearsals use `-rc.N` tags. Tags are signed,
never move and are never deleted, so a failed release takes the next patch number.

## Support

Only the latest release receives fixes. No support objective has been established.

## Release artifacts

| Artifact | Where |
|---|---|
| Control plane image | `ghcr.io/dac-plane/dac-plane` |
| Adapter images | `ghcr.io/dac-plane/dac-codex-adapter`, `dac-e2b-adapter`, `dac-s3-adapter` |
| Adapter kit | Go module `github.com/dac-plane/dac-plane/adapterkit` |
| Image digests, SBOMs, provenance, scan results, Compose stack, contracts | Assets of each [GitHub Release](https://github.com/dac-plane/dac-plane/releases) |
| Release notes | [docs/releases](docs/releases/README.md), repeated on the GitHub Release |

## Verifying a release

`release-manifest.json`, attached to each release, lists every image digest. Deploy by digest, and check what you pull:

```sh
gh attestation verify oci://ghcr.io/dac-plane/dac-plane@sha256:DIGEST --repo dac-plane/dac-plane
```
