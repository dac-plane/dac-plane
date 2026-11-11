# Publish dac-plane v0.2.0

Tracker: https://github.com/dac-plane/dac-plane/issues/134
Branch: `chore/release-v0.2.0`, from `main` at `3d6f640`.

## Where the tree stands

Surveyed on 2026-11-11.

| Finding | Evidence | Consequence |
|---|---|---|
| `adapterkit/` differs from `adapterkit/v0.1.0` | `git diff adapterkit/v0.1.0 HEAD -- adapterkit`: the #123 session fix, the token error wording, dependency updates | `scripts/release/check-tag.sh` requires the kit tag to hold exactly this tree, so the kit needs a new version. |
| The kit's Go API is unchanged | The diff adds only unexported names | Kit `v0.1.1`, a patch. |
| The control plane gained a command and a migration | #115, schema 16 | Control plane `v0.2.0`. |
| The pilot overlay pins v0.1.0 digests | `deploy/compose/compose.pilot.yaml` | Stays as it is. Upgrading the pilot is separate work, and v0.2.0's digests do not exist before the release. |

## Decisions

- The notes state the upgrade order from v0.1.0 and the rollback limit migration 16 creates.
- The evidence note is `docs/architecture/release-134.md`. It starts with the live run of 2026-11-09 and gains the
  rehearsal and release sections as they happen.
- The pin in the `justfile` advances in a second pull request, after `just release-verify v0.2.0`.

## Steps

1. Notes, the kit requirement, the kit version in the two guides, the evidence note. Verify: `just ci-fast`,
   `just docs-check`. Merge.
2. `just release-tag v0.2.0-rc.1`. Verify: the release workflow passes and drafts a prerelease.
3. `just release-qualify v0.2.0-rc.1` from a checkout of the tag. Record every run.
4. `just release-tag v0.2.0` on the qualified commit. Verify: the workflow publishes the release.
5. `just release-verify v0.2.0`. Record digests and results.
6. Advance `previous_release` and `previous_release_image`; record the evidence. Verify:
   `just test-operations-upgrade`.
