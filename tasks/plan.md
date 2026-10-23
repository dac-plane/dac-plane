# Issue #69 — Refresh the container view after M1

Tracker: https://github.com/dac-plane/dac-plane/issues/69
Branch: `docs/69-container-view-refresh`, from `main` at `f84b1f3`.
Parent: #65 (M2: Deployable control plane).

## Direction

Bring `docs/architecture/container-view.drawio`, its exports and the baseline README in line with M1
completion and the accepted M2 decisions. No decision changes here; every change traces to an accepted
ADR or to M1 evidence:

- M1 is complete ([contract freeze](../docs/architecture/contract-freeze-30.md)): the
  "build first with deterministic simulator" and "prototype schema is incomplete" labels are stale.
- ADR-0007: surfaces use the Surface API over HTTPS with replayable events.
- ADR-0008: every provider role, including the artifact store, is reached through an out-of-process
  adapter that opens a TLS WebSocket carrying JSON ControlFrames; in-process simulators are for tests
  only; providers never write PostgreSQL.
- ADR-0009: deployment detail belongs in the deployment view, not this one.

## Implementation

- [x] Read the current view and the README prose that describes it.
- [x] Edit `container-view.drawio`: remove stale labels, name the Surface API and adapter endpoint,
  add the provider-adapter container outside the system boundary, and move artifact storage behind it
  with the other provider roles.
- [x] Re-export `container-view.svg` and `container-view.png` from the edited source; inspect the
  render for overlaps and kinks.
- [x] Update the README: the containers section prose and audience, and the stale statement that the
  schema is incomplete and the contract freeze is the first task.
- [x] Open the PR stating what changed and why.

## Verification

- `git diff --check`
- Exports regenerated from the committed source with the draw.io CLI.
- Every relative link touched in the README resolves.

## Remaining boundaries

The responsibility map is unchanged: ownership did not change. No provider is selected, and no
deployment detail is added to this view.
