# Issue #101 task cancellation qualification evidence

Date: 2026-10-30. Host architecture: Darwin arm64. Result: PASS for the bounded scenarios below on the
deployed Surface API and ControlFrame boundaries. Disposition: both providers stay `off`.

**This does not prove that E2B stopped the work.** Codex runs in its adapter container and receives the
sandbox reference only as opaque text, so the command and the sandbox were independent. The evidence
proves that two independent stops converge. Running Codex inside the environment is separate work and
must land before the #96 pilot claims containment.

## Pins

- Codex app-server `0.165.0`, model `deepseek-v4-flash-0731` through Camer.
- Adapter images, Codex then E2B: `sha256:7b4a885634b9645361f295b3eca92d0c5e75bd55107e444acccbe5222a18716e`, `sha256:e29eca3abfdb6c3667e81f2da0c2093a01ae9d90921cda622244e8f78588d8e4`.
- DAC revision `6119812`. E2B Cloud template `base`, sandbox lifetime 15 minutes.

## Command

```sh
DAC_CODEX_API_KEY_FILE=... DAC_E2B_API_KEY_FILE=... just qualify-task-cancellation
```

The credentials were held in ignored files for the run and then removed. The recipe's check found
neither in the compose logs.

## Observed

Each scenario ran a real command, confirmed it in the adapter's process list and confirmed the sandbox
was running before cancelling. Each then required the task `cancelled`, the attempt `terminated` with
both stops, the reservation `released`, the command gone, E2B no longer reporting the sandbox as
running, and exactly one applied stop of each kind.

Five full runs were made. Four passed all seven scenarios, in 251 to 271 seconds each. Run 1 failed as
recorded below.

| Scenario | How it is forced |
|---|---|
| Active command, then restart everything | Cancel during the command, restart all three processes afterwards |
| Environment adapter down | Stop the adapter, cancel, start it |
| Control plane restart | As above, with `dac-plane` restarted while the stop is pending |
| Runtime adapter down | Stop the adapter, cancel, start it |
| Interrupt lost | Pause the adapter, cancel, kill it with the frame unread, start it |
| Start lost | Pause the adapter, submit until the start is uncertain, cancel, kill, start |
| Stop lost | Pause the adapter, cancel until the stop is uncertain, kill, start |

Scenarios converged in about 25 to 36 seconds from submission. The stop lost scenario took about 67
seconds, because it waits out the 30-second send timeout and the reconcile interval.

- **Order.** The runtime stop landed first in every run. In the two environment scenarios the task
  stayed `cancelling` and the sandbox stayed running until the adapter returned.
- **Restart after convergence.** Restarting all three processes changed no state, version, reservation
  or applied audit count over the following 30 seconds.
- **Duplicates.** Each run records one `stop_already_observed` refusal, because Codex reports both
  `turn.finished` and `runtime.stopped`. It changes nothing.
- **Codex restart.** After the adapter process was killed, `thread/resume` reported the turn as ended, so
  the stop arrived without an interrupt reaching a live turn. A repeated interrupt returned
  `no active turn to interrupt`, which is harmless.

## Failures kept

1. A first spike stopped after 15 seconds on a token request timeout before any task ran. It is invalid
   and proves nothing. The same request succeeded on every later run; the cause was not established.
2. The first full run failed: the task reached `cancelled` while `sleep 317` kept running. Later
   scenarios then failed because the leftover process matched their readiness check.
3. A focused rerun confirmed it was not timing: `sleep 3171` was alive under `codex-app-server` 60 seconds
   after convergence. `turn/interrupt` ends the turn, not its command. The adapter now calls
   `thread/backgroundTerminals/clean` first, which the pinned binary gates behind `experimentalApi`.
   That API is experimental and needs rechecking at every Codex pin change.
4. The first stop lost run failed: the task stayed `cancelling` for the full five minutes. A frame that
   dies unread is never journaled, so adapter retries could not finish it. The worker now replays it to
   a provider that negotiated `operation.replay`.
5. Run 1 of the five full runs failed and **its cause is unknown**. The active command scenario passed,
   then the next scenario's command never appeared within the 38 minutes the wait had left. The wait had
   no deadline and no check for an ended task, and the next run overwrote the logs. Both are fixed. The
   three runs since passed, which shows nothing about what happened. A model that never issued the
   command would look the same, and so would a stuck task.

## Limits

- Four passes in five runs on one host, with one unexplained failure. Nothing here is an availability,
  cost or production claim.
- Only one command per thread was exercised, and only a plain `sleep` that holds no descriptors.
- `thread/backgroundTerminals/clean` is reported complete by its RPC reply. The process list check, not
  the reply, is the evidence.
- A failed E2B `DELETE` is proved by deterministic tests only. The adapter allows plain HTTP only on
  loopback, so a live fault proxy was not used.
- The live fault scenarios kill the adapter, which also ends the Codex command. The repeat of an
  interrupt to a live adapter holding a running turn is proved by deterministic tests.
- Stale observations are proved by integration tests. Restarting a real adapter replays none.
