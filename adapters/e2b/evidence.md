# E2B Embed qualification evidence

Status: incomplete; provider is not qualified. This note separates observed local evidence from the live provider gates that remain unrun.

## Pins observed from source

- E2B Embed source: `7a69f45621f864495695e698dc5a2b93a56d2d52` (2026-10-29).
- Runtime source pinned by that Embed package: `7278c2a380767c9989da73cf4c04b1af1b32da18`.
- E2B Runtime release `2026.30` resolves to `f32ee8a2a50052f32e3632ceb451111a98dd5104`; it does not contain `embed/`.
- E2B service image tags and image digests: not yet recorded; the Linux/KVM qualification recipe has not run.

## Deterministic observations

Host/tool versions observed on 2026-10-29: Go `go1.26.10 darwin/arm64`; Docker client/server `29.8.2`, server `linux/arm64` under OrbStack; E2B CLI `2.23.0` with SDK `2.61.0` pinned by the qualification lockfile.

Commands run: `just test-e2b-adapter`; `go test -tags=e2bqualification -run '^$' ./...`; `docker buildx build --load --target dac-e2b-adapter -t dac-e2b-adapter:dev .`; `just ci-fast`; `git diff --check`.

Observed result: the E2B adapter unit/race tests and adapter-kit ControlFrame conformance suite passed against the deterministic fake E2B API. Coverage includes migration request validation, changed-payload rejection, lost create replies, malformed reconciliation responses without a second create, journal restart reconciliation, duplicate observation replay and sequence persistence, stop and cancellation behavior, stopped-state observation, and credential redaction. The tagged live qualification test compiles. The adapter image built locally with image manifest digest `sha256:5902d10cee018d3dbc88afc9eb3868b6d5d1cb37aca7af1d008ff86047f863a2`; this is the locally built adapter image, not an E2B service image digest.

`just ci-fast` passed. A subsequent `just ci` run was interrupted during `test-integration-other-race` after more than eight minutes without package results. This is incomplete validation, not a reported test failure or a pass.

This does not prove Firecracker isolation, process or filesystem separation, E2B egress enforcement, host port isolation, actual resource cleanup, or behavior against the pinned Embed images.

## Live qualification still required

`just qualify-e2b-environment` was attempted locally and exited before setup with `E2B Embed qualification requires Linux with /dev/kvm`. No E2B Embed containers were pulled or started, and no provider service image digest was observed. Run it on a disposable Linux host with KVM, Docker Engine 27+, Compose 2.24+, the required memory/disk, and a private host firewall. Record the host OS/kernel, Docker and Compose versions, pulled OCI digests, adapter revision, exact commands, elapsed time, two-sandbox filesystem/process isolation results, denied egress probe, stop observations, adapter restart reconciliation, resource cleanup, failures, and final off/shadow/live disposition here.

The current workspace host is macOS/OrbStack and does not establish the required Linux KVM host. Provider disposition remains off/unqualified; no live provider result or production qualification claim is made.
