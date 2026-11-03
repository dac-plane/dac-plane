# Provider qualification evidence — issue #75

Date: 2026-11-03. Host: Darwin arm64 with Docker 29.8.2. Result: PASS for the bounded scenarios below on the
deployed Surface API and ControlFrame boundaries.

Disposition: **live** for Codex app-server 0.165.0, E2B Cloud and the bundled SeaweedFS profile, used together,
within the limits below. This is not a production or availability claim. Every other provider stays `off`.

The promotion rule in the [#91 note](codex-runtime-91.md) also asks for a registry image digest. Nothing is
published yet, so by the project owner's decision that requirement moves to the packaging issue,
[#95](https://github.com/dac-plane/dac-plane/issues/95). The images were local builds. See
[Identifying an image](#identifying-an-image).

## Pins

| Item | Value |
|---|---|
| Codex | app-server 0.165.0 |
| Sandbox template | `dac-codex-exec` (`th79mhkayk88pgbw6ijn`), built on 2026-11-01 from [`e2b.Dockerfile`](../../adapters/e2b/template/e2b.Dockerfile), envd 0.10.0 |
| Artifact store | SeaweedFS 4.51, pinned in [`compose.artifacts.yaml`](../../deploy/compose/compose.artifacts.yaml) |
| Model | Camer `deepseek-v4-flash-0731` |

E2B Cloud's own build is not exposed or pinnable.

## Command

```sh
DAC_E2B_TEMPLATE_ID=dac-codex-exec just qualify-providers
```

It needs `DAC_CODEX_API_KEY_FILE` and `DAC_E2B_API_KEY_FILE`. It fails if a simulator process is running, if the
runtime, environment or artifact store adapter did not connect, or if a provider key appears in the Compose logs.

## Runs

Every run is recorded, including those that failed.

| Run | Scope | Result | Cause |
|---|---|---|---|
| 1 | Full | Stopped after about 32 minutes; all 7 cancellation scenarios and the first 4 containment scenarios had failed | Every task stayed in `provisioning`: the adapter bug below. |
| 2 | Full | 15 of 15 scenarios passed in 680.3 seconds; the recipe exited 1 | The recipe's final adapter check failed falsely, as described below. |
| 3 | One scenario | Passed in 33.7 seconds; the recipe exited 0 | Proved the corrected check exits cleanly. |
| 4 | One scenario, E2B's `base` template | The task failed as intended, in 22.9 seconds | `base` has no Codex, so this forced a failed start after the stall fix below. |
| 5 | A result is read from the sandbox | Passed in 59.8 seconds; the recipe exited 0 | Final code: the renamed module and the stall fix. |

## Final run

Run 2 is the only full run. Afterwards the recipe's final check changed, the Go module was renamed and the stall
below was fixed, and runs 4 and 5 covered those changes with one scenario each. The Compose log of run 2 shows all
three adapters connected and no provider key.

| Scenario | Time | Observed |
|---|---|---|
| Active command | 82.6 s | Cancelled; both stops, released reservation, command gone. |
| Environment adapter down | 41.5 s | The task waited for the environment stop and converged after the adapter returned. |
| Control plane restart | 44.3 s | Converged. |
| Runtime adapter restart | 39.9 s | Converged. |
| Interrupt lost with its adapter | 43.8 s | Converged. |
| Start lost with its adapter | 36.9 s | Converged. |
| Stop lost with its adapter | 66.9 s | Converged. |
| Commands run in the sandbox | 34.0 s | The command was in the sandbox and not in the adapter container. |
| Credentials stay out of the workload | 35.0 s | The search reached the workload's own command line and found no model, attach, token or E2B secret. |
| Egress stays denied | 51.4 s | The model endpoint and `1.1.1.1` were unreachable from the sandbox. |
| Stopping the environment alone | 60.1 s | A direct E2B delete failed the task and sent no interrupt. |
| Runtime restart | 27.6 s | The task failed, the command ended and the sandbox was deleted. |
| A result is read from the sandbox | 46.9 s | A file written in the sandbox was committed in SeaweedFS as exactly one version with its exact bytes, and the task reached `succeeded`. |
| An open approval survives a control plane restart | 40.1 s | Approving after the restart produced the exact result, and the task reached `succeeded`. |
| A runtime restart ends an open approval | 29.0 s | The owner approved while the Codex adapter was down. The Attempt failed, the budget was released, no attention stayed open and `unapproved.txt` was not in the sandbox. |

These last two scenarios close the gap the [#91 note](codex-runtime-91.md) left open for an unresolved native
approval. The search for `unapproved.txt` covered the sandbox except `/proc`, and had no positive control.

## What the qualification changed

- **Live Codex on E2B could not provision on `main`.** The #108 run accepted any answer but 502 from exec-server.
  `67d078a` then required 401, after that run, and a fake that always answered 401 hid the mismatch: exec-server
  answers a plain request with 400 before it checks a token. So every sandbox was deleted after 30 seconds, and
  the [#108 note](codex-containment-108.md) did not describe what was merged. The adapter now sends a WebSocket
  upgrade, which draws the 401, and the fake refuses a request without one.
- **A failed sandbox start stalled silently.** The adapter's 30-second wait for exec-server equalled the control
  plane's 30-second send timeout, and its error reply meant an unknown outcome. The control plane never dispatches
  an uncertain operation again, so the task stayed in `provisioning`, and the adapter logged nothing. The wait is
  now 20 seconds, and a sandbox known to be deleted is answered as rejected. In run 4 the task failed with the
  reason in the E2B adapter's log. The adapter kit logs every failed reply's code, not its message.
- **An upload failure could expose its capability.** Go's HTTP errors quote the URL, so a failed upload returned
  the presigned URL in the Codex adapter's reply to the control plane, and for a short time in its log. The adapter
  now drops the URL from those errors.
- **A recipe check failed falsely.** `compose logs | grep -q` fails under `pipefail` when `grep` closes the pipe
  first. `qualify-providers` and `qualify-codex-artifact-store` now read the logs before searching them.

## Identifying an image

Image IDs from the recipe changed between runs 2 and 3, so they were investigated. Two local builds of one tree
gave identical layers and different image IDs, and the local Docker uses the containerd image store. So a local image
ID does not identify content, and `qualify-providers` now records each image's top layer. `just image-repro`
built all five images twice with identical digests from one tree, which is the path #95 should publish from.

## Limits

- One passing full run on one host, arm64 for the adapters, with local images. Nothing here is an availability,
  throughput, cost or production claim.
- **A provision can still stall.** If its reply is truly lost, or the sandbox cannot be confirmed deleted, the
  operation stays `uncertain` and the task stays in `provisioning` until someone cancels it. A create slow enough to
  use the remaining 10 seconds of the send timeout does the same.
- The E2B log showed `Process start (server stream end)` at `error` level during run 2, at about the time of the
  egress scenario. Its cause was not established. The run passed.
- The limits in the [#108](codex-containment-108.md), [#101](task-cancellation-101.md) and
  [#93](artifact-store-93.md) notes still apply.
