# Provider qualification evidence — issue #75

Date: 2026-11-03. Host: Darwin arm64 with Docker 29.8.2. Result: PASS for the bounded scenarios below on the
deployed Surface API and ControlFrame boundaries.

Disposition: **live** for Codex app-server 0.165.0, E2B Cloud and the bundled SeaweedFS profile, used together,
within the limits below. This is not a production or availability claim. Every other provider stays `off`.

The promotion rule in the [#91 note](codex-runtime-91.md) also asks for a registry image digest. Nothing is
published yet, so by the project owner's decision that requirement moves to the packaging issue,
[#95](https://github.com/dac-plane/dac-plane/issues/95). The images below are local builds, identified by image ID.

## Pins

| Item | Value |
|---|---|
| Codex | app-server 0.165.0 |
| Sandbox template | `dac-codex-exec` (`th79mhkayk88pgbw6ijn`), built on 2026-11-01 from [`e2b.Dockerfile`](../../adapters/e2b/template/e2b.Dockerfile), envd 0.10.0 |
| Artifact store | SeaweedFS 4.51, pinned in [`compose.artifacts.yaml`](../../deploy/compose/compose.artifacts.yaml) |
| Model | Camer `deepseek-v4-flash-0731` |
| Control plane image | `sha256:71bdcb7abae5664eb31f16a392b4d35d919153beffd4b358b6f69ced6a1e5c26` |
| Codex adapter image | `sha256:31f48f242a1673ba3aa8bd18238db8733e82273a1b6653bfa96aad0e06013db2` |
| E2B adapter image | `sha256:6902cbfc4503c87b3ff3368342f08069bbaff3ad9bb08abbcc8d0a15c77a7e9e` |
| S3 adapter image | `sha256:ff4b2cc1f77092ac542095f52457c5d6db0b802f2987351ca3ff795e563005cb` |

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

## Final run

Run 2 used the stack and the adapter fix recorded here, and the pins above are its images. Afterwards, the recipe's
final check and these documents changed, but no Go source. Run 3's four image IDs differ from run 2's; why was not
investigated, so the pins identify run 2 only. The Compose log of run 2 shows all three adapters connected and no
provider key.

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
- **A recipe check failed falsely.** `compose logs | grep -q` fails under `pipefail` when `grep` closes the pipe
  first. `qualify-providers` and `qualify-codex-artifact-store` now read the logs before searching them.

## Limits

- One passing full run on one host, arm64 for the adapters, with local images. Nothing here is an availability,
  throughput, cost or production claim.
- **A failed sandbox start stalls silently.** The adapter's 30-second wait for exec-server equals the control
  plane's 30-second send timeout. When the wait fails, the operation becomes `uncertain`, the task stays in
  `provisioning` until someone cancels it, and the adapter logs nothing.
- The E2B log showed `Process start (server stream end)` at `error` level during run 2, at about the time of the
  egress scenario. Its cause was not established. The run passed.
- The limits in the [#108](codex-containment-108.md), [#101](task-cancellation-101.md) and
  [#93](artifact-store-93.md) notes still apply.
