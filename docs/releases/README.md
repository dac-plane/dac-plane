# Release notes and procedure

## Notes

- [v0.1.0](v0.1.0.md)

Copy [template.md](template.md) for a new release. The GitHub Release itself holds only a link to these notes and
every issue resolved since the last release, grouped by kind with its area as a tag. So close issues as completed and
give them a conventional title.

## Cutting a release

You need a signing key that GitHub reports as verified.

1. **Prepare `main`.** Merge the notes as `docs/releases/vX.Y.Z.md`, link them above, and set the kit version in every
   `go.mod`, without an `-rc` suffix. [`release.yml`](../../.github/workflows/release.yml) checks both.
2. **Rehearse.** `just release-tag v0.1.0-rc.1`. The workflow publishes to `ghcr.io/dac-plane/dry-run` and drafts a
   prerelease.
3. **Qualify the rehearsal live.** From a checkout of the rc tag, run `just release-qualify v0.1.0-rc.1`. It needs
   `DAC_CODEX_API_KEY_FILE`, `DAC_E2B_API_KEY_FILE` and `DAC_E2B_TEMPLATE_ID`, and it spends provider credit. Record
   every run in the release's evidence note, including failed ones. A failure that the notes already list as a known
   limit may be rerun alone. Any other failure stops the release.
4. **Tag the release.** `just release-tag v0.1.0`, on `main`, from the commit you qualified. If the gate passes, the
   release publishes itself with nothing further to approve.
5. **Check it as a stranger would.** From a checkout of the tag, run `just release-verify v0.1.0`. Record the digests
   and results in the evidence note.
6. **Advance the upgrade test's pin.** Set `previous_release` and `previous_release_image` in the
   [`justfile`](../../justfile) to the new version and the `dac-plane` digest from its `release-manifest.json`, and
   merge that once `just test-operations-upgrade` passes. From then on every pull request upgrades from this release.

A package new to this repository starts private, so the clean room's login passes and an anonymous pull fails. Make it
public in [its settings](https://github.com/orgs/dac-plane/packages) when you add a new image.

## When something fails

| What failed | What to do |
|---|---|
| A rehearsal | Fix it on `main` and tag the next `-rc.N`. Delete its draft if it has one. |
| A job, because of the runner or network | Rerun the failed jobs. Images are reproducible, so the digests must match. |
| The scan, with a fix available | Update the dependency or base image. |
| The scan, with no fix | Add the finding to `.trivyignore.yaml`, with a reason and an expiry date. |
| A final tag | Do not retag. Fix `main` and release the next patch, and say in its notes why the number was skipped. |
| A published release | Publish the next patch, and state the problem in its notes. |
