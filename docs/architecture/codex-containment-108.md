# Issue #108 containment qualification evidence

Date: 2026-11-01. Host: Darwin arm64 with Docker under OrbStack. Result: PASS for the bounded scenarios below on the
deployed Surface API and ControlFrame boundaries. Disposition: both providers stay `off`; this note does not
change a provider's mode.

Codex's commands now run in the sandbox assigned to their Attempt, and stopping that sandbox ends the work. The
design and its spike observations are in [ADR-0016](../adr/0016-run-agent-commands-inside-the-environment.md).

## Pins

| Item | Value |
|---|---|
| Revision of the final run | `af85ac706da39673743d5fd97835c0f38fcd4e17` |
| Codex | app-server and CLI 0.165.0, each archive verified by SHA-256 |
| Sandbox template | [`e2b.Dockerfile`](../../adapters/e2b/template/e2b.Dockerfile), built by the pinned E2B CLI 2.23.0 as `dac-codex-exec` |
| Template base | `e2bdev/base@sha256:3638f4332f0a8517bebcff5ebf149c1909263cd8fd77c0eff3a42b7d8ab7f47c` |
| Codex adapter image | `sha256:21f4d5724f7cdcde2d309455ba66638f9ee586d49741c712a4c7b08b6297f82e` |
| E2B adapter image | `sha256:deb553243b57bade4b13eb36134b50fbb516987b61f72e5567fb06ddfdb70aac` |
| Model | Camer `deepseek-v4-flash-0731` |

E2B Cloud's own build is not exposed or pinnable.

## Commands

```sh
just build-e2b-codex-template
DAC_E2B_TEMPLATE_ID=dac-codex-exec just qualify-codex-containment
just qualify-e2b-environment
just test-codex-adapter
just test-e2b-adapter
just test-conformance
just ci-fast
```

## Runs

Every run is recorded, including those that failed.

| Run | Scope | Result | Cause |
|---|---|---|---|
| 1 | Containment | 3 of 6 passed | The credentials search missed NUL-separated command lines. Two scenarios timed out; no retained state showed why, and they did not recur. |
| 2 | 4 containment scenarios | Invalid | The Docker VM stalled: PostgreSQL, the API and all adapters stopped answering and recovered in the same second. No product conclusion is drawn. |
| 3 | 4 containment scenarios | 2 of 4 passed | Found the `waiting_input` bug below. The restart scenario checked before the queued environment stop landed. |
| 4 | Containment and cancellation | 12 of 13 passed | The harness failed on envd's 502 from a sandbox being deleted. |
| 5 | Containment and cancellation | 13 of 13 passed | Final run, 611.6 seconds. |

Run 1's timeouts remain unexplained, as #101's failure 6 did until a race was found. Later runs did not repeat
them, which is not proof that they cannot recur.

## Final run

Every proof reads the sandbox through E2B, not through the adapters under test.

| Acceptance criterion | Scenario | Observed |
|---|---|---|
| Commands execute in the assigned sandbox | Commands run in the sandbox (33.9s) | The unique `sleep` was found in the sandbox and not in the adapter container, on host `e2b.local`, beside exec-server. |
| Stopping the environment alone ends the work | Stopping the environment alone (65.4s) | A direct E2B DELETE with no cancellation: the sandbox was gone, the task failed in 65.3 seconds, the reservation was released and no interrupt was sent. |
| Credentials are not readable by the workload | Credentials (30.0s) | About 2 MB of every process's environment and command line and readable files was searched outside the sandbox. It held the token hash and the workload's command, which proves the search reached them, and none of the model key, attach key, attach token or E2B key. |

Other scenarios:

| Scenario | Observed |
|---|---|
| Egress stays denied (44.9s) | The model endpoint and `1.1.1.1` were unreachable from the sandbox while Codex used the model from the adapter. |
| Runtime restart (38.6s) | Killing the adapter failed the task, and the environment stop then ended the command and deleted the sandbox. |
| A result is read from the sandbox (41.6s) | A file written in the sandbox arrived as an artifact with exact bytes, and the task reached `succeeded`. |
| #101 cancellation, seven scenarios (356.5s) | All passed with the work in the sandbox, including both stops, a released reservation and a command gone within about 350 ms of convergence. |

`just qualify-e2b-environment`, `just test-conformance` (14 scenarios), `just ci-fast`, the application and worker
integration suites and the credential scan of the stack logs also passed.

## What the qualification changed

- **A task could wait forever on a failed Attempt.** A failed Attempt told its Task `execution_failed`, which
  `waiting_input` did not accept, so a sandbox lost with an approval open left the task stuck after its budget
  was released. Nothing else fired for that wait. The transition now exists, with an integration test.
- **A lost sandbox read as success.** Observed in the spike, the model retried and finished its turn as
  `completed`. A turn that lost its environment is now reported failed, and so is an interrupted turn found after
  a restart, since an unrequested `interrupted` would otherwise leave the Attempt waiting.
- **The exec-server address is validated.** A reference that could change the host would have been handed a
  bearer token, so it is refused at start.

## Limits

- One passing full run on one host. Nothing here is an availability, cost or production claim.
- Only a plain `sleep` and a small file were exercised. Codex's own processes and tools were not fuzzed.
- The workload is not prevented from reaching exec-server's local port. It holds no token, so it is refused, but
  it runs as the same user and can kill the server, which ends its own work and no other task's.
- The port's only access control is the bearer token, and E2B terminates its TLS. See ADR-0016.
- After a runtime restart, a turn can be reported ended while its command runs on for up to about 40 seconds,
  until the environment stop lands. That report alone is not proof that the command ended.
- A lost sandbox takes about a minute to end its task, because Codex reported the lost link after 26 seconds in
  the spike and the adapter then waits 10 seconds for it to return.
- The E2B adapter holds its lock while exec-server starts, for up to 30 seconds, so other operations wait.
- Results are read whole, at most 128 MiB per file, and a snapshot reads every workspace file over the link.
- `environment/add`, `thread/start.environments` and exec-server are experimental Codex APIs.
- A template named `dac-codex-exec-spike` from the spike remains on the E2B account and is not used.
