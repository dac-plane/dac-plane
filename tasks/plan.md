# Upgrade from the last published release in the operations suite

Tracker: https://github.com/dac-plane/dac-plane/issues/116
Branch: `test/operations-upgrade-from-release`, from `main` at `2c3019f`.

## Where the tree stands

Surveyed on 2026-11-11.

| Finding | Evidence | Consequence |
|---|---|---|
| The old side of the upgrade is built from source | `justfile`, `operations-images`: `git archive ca20b3e…` then `docker build` | Pull the published image by digest and tag it `dac-plane:operations-old`. No archive, no old build. |
| v0.1.0's image is public and multi-architecture | An anonymous `docker manifest inspect` of the manifest's digest lists amd64 and arm64 | CI and a Mac pull the same pin with no login. |
| v0.1.0 serves schema 13 to 15 | `docker run … version`: `schema_min=13`, `schema_current=15` | Its own `migrate up` leaves the database at 15; the tree's real 16 and the probe 17 are both ahead of it. |
| v0.1.0 accepts today's submission body | `git show v0.1.0:internal/httpapi/bodies.go` | `submitLegacy` described `ca20b3e`'s older body. The scenario uses `submit` and the helper goes. |
| `provision` does not gate on the schema range, but the suite's current image is not the binary that owns schema 17 | `internal/provision`; `compose.dev.yaml`'s `provision-tenant` uses `DAC_PLANE_IMAGE` | Add a `provision-next` service on the next-release image for the post-migration command. |
| CI fetched full history only for the archive | `.github/workflows/ci.yml`, the `operations` job | Drop `fetch-depth: 0`. |

## Decisions

- The pin is two justfile variables, the version and the image reference by digest, copied from that release's
  `release-manifest.json`. After the pull the recipe checks that the image reports the pinned version, so a digest
  and a version that disagree fail before the suite starts.
- The real schema 16 is exercised alongside the probe, in one roll: v0.1.0 at 15, replicas to the next-release
  image, `migrate up` to 17. Keeping the probe means the scenario still crosses a migration when the pin advances
  to a release whose schema equals the tree's.
- After the migration the scenario runs `provision set-policy` and asserts the generation v0.1.0 wrote (1) advanced
  to 2 with one recorded `set_policy` change.
- `docs/architecture/operations-74.md` is dated evidence of the `ca20b3e` run and stays as it is.
- Advancing the pin is a new last step of the release procedure, after the release is verified.

## Steps

1. `operations-images` pulls and checks the pin. Verify: the recipe runs with no `docker build` of old source.
2. The scenario: `submit`, the policy assertions, `provision-next`. Verify: `just test-operations-upgrade`.
3. Release procedure, compose README, operate guide, CI checkout. Verify: `just docs-check`, `just lint-actions`
   or its equivalent.
4. `just test-operations`, `just ci-fast`, `git diff --check`, PR.
