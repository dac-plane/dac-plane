# Release notes and procedure

## Notes

- [v0.4.0](v0.4.0.md)
- [v0.3.0](v0.3.0.md)
- [v0.2.0](v0.2.0.md)
- [v0.1.0](v0.1.0.md)

Copy [template.md](template.md) for a new release. The GitHub Release itself holds only a link to these notes and
every issue resolved since the last release, grouped by kind with its area as a tag. So close issues as completed and
give them a conventional title. The change list release-please writes is in [CHANGELOG.md](CHANGELOG.md), which it
creates with the first release pull request.

## Cutting a release

A release is decided by merging the release pull request that [release-please](https://github.com/googleapis/release-please)
opens and keeps current from the conventional commits on `main`. It carries the version bump, the change list and the
adapter kit's version in every `go.mod`, so no version is edited by hand. The root module and the adapter kit are
versioned together, so `vX.Y.Z` and `adapterkit/vX.Y.Z` always name the same commit.

1. **Write the notes in the release pull request.** release-please writes the change list. You add
   `docs/releases/vX.Y.Z.md` (copy [template.md](template.md): upgrading, known limits, compatibility) and link it in
   the list above. The `Release notes` check fails the pull request until both exist.
2. **Rehearse (optional).** From the release pull request's head, `just release-tag vX.Y.Z-rc.1`. This path keeps its
   signed tag: you need a signing key that GitHub reports as verified. The workflow publishes to
   `ghcr.io/dac-plane/dry-run` and drafts a prerelease.
3. **Qualify the rehearsal live (optional).** From a checkout of the rc tag, run `just release-qualify vX.Y.Z-rc.1`. It
   needs `DAC_CODEX_API_KEY_FILE`, `DAC_E2B_API_KEY_FILE` and `DAC_E2B_TEMPLATE_ID`, and it spends provider credit.
   Record every run in the release's evidence note, including failed ones. A failure that the notes already list as a
   known limit may be rerun alone. Any other failure stops the release.
4. **Merge the release pull request.** Merging it is the decision to release. Only the repository owner can merge it,
   and only with the checks green (branch protection). The [`release-please`](../../.github/workflows/release-please.yml)
   workflow then tags the merge commit `vX.Y.Z` and `adapterkit/vX.Y.Z` and runs the release workflow on it. Those tags
   are not signed. What stands in for the signature is that
   [`check-tag.sh`](../../scripts/release/check-tag.sh) stops the release unless both tags name the merge commit of a
   merged release-please pull request on `main` whose manifest names this version. The images keep their provenance and
   SBOM attestations. If the gate passes, the release publishes itself with nothing further to approve.
5. **Check it as a stranger would.** From a checkout of the tag, run `just release-verify vX.Y.Z`. Record the digests
   and results in the evidence note.
6. **Advance the upgrade test's pin.** Set `previous_release` and `previous_release_image` in the
   [`justfile`](../../justfile) to the new version and the `dac-plane` digest from its `release-manifest.json`, and
   merge that once `just test-operations-upgrade` passes. From then on every pull request upgrades from this release.

A signed tag pushed by hand with `just release-tag vX.Y.Z` still publishes a final release, as it did before release-please,
but then the manifest and every `go.mod` are yours to keep in step. Use it for a rehearsal, or if the release pull request
cannot be used.

### What a release holds

The GitHub Release lists `release-manifest.json` (the image digests and the kit version), the compose and contracts bundles,
and `dac-plane-evidence-vX.Y.Z.tar.gz`: the vulnerability scan, the SBOM and the provenance of each image, for an offline
audit. The attestations themselves are in GitHub's attestation store and the registry, so
`gh attestation verify oci://ghcr.io/dac-plane/dac-plane:vX.Y.Z --repo dac-plane/dac-plane` checks them without these
files.

### One-time setup

- **Allow Actions to open pull requests.** Settings, Actions, General, "Allow GitHub Actions to create and approve pull
  requests". Without it release-please cannot open its pull request.
- **Let the release pull request run its checks.** A pull request opened with the built-in token starts no workflow, so
  its checks do not run. Add a `RELEASE_PLEASE_TOKEN` secret, a fine-grained personal access token or a GitHub App token
  that can write contents and pull requests, and the workflow uses it. Without it, close and reopen the release pull
  request, or push an empty commit to its branch, to start the checks.
- **Protect `main`.** Require the checks and limit who can merge to the owner, which is what makes the merge a decision.
- A package new to this repository starts private, so the clean room's login passes and an anonymous pull fails. Make it
  public in [its settings](https://github.com/orgs/dac-plane/packages) when you add a new image.

## When something fails

| What failed | What to do |
|---|---|
| A rehearsal | Fix it on `main` (and so in the release pull request) and tag the next `-rc.N`. Delete its draft if it has one. |
| A job, because of the runner or network | Rerun the failed jobs. Images are reproducible, so the digests must match. |
| The clean room, because the module proxy does not serve the new kit version | A lookup made before the kit tag existed, such as a dependency scan of `main`, is remembered for a while. The clean room waits up to 45 minutes. If it still fails, rerun the failed job later. Do not retag. |
| The scan, with a fix available | Update the dependency or base image. |
| The scan, with no fix | Add the finding to `.trivyignore.yaml`, with a reason and an expiry date. |
| A final release after its tag exists | Rerun the failed jobs of the `release-please` run that started it; the tag is not made again. If the fault is in the code, do not retag: fix `main` and let release-please open the next patch, and say in its notes why the number was skipped. |
| A published release | Publish the next patch, and state the problem in its notes. |
