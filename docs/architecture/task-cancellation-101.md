# Issue #101 task cancellation qualification evidence

Date: 2026-10-30. Host architecture: Darwin arm64. Result: PASS for the bounded scenarios below on the
deployed Surface API and ControlFrame boundaries. Disposition: both providers stay `off`.

**This does not prove that E2B stopped the work.** Codex runs in its adapter container and receives the
sandbox reference only as opaque text, so the command and the sandbox were independent. The evidence
proves that two independent stops converge. Running Codex inside the environment is separate work and
must land before the #96 pilot claims containment.

## Pins

- Codex app-server `0.165.0`, model `deepseek-v4-flash-0731` through Camer.
- Adapter images, Codex then E2B: `sha256:1ccfb40115a81d0551fc746568be9c86c67acf8a7ac09ba5afb6263dafd4d033`, `sha256:f4a32bcc8dfdc28957f45b857d51188f9f6e1d2a398e71eb6aaed9448a0f7d82`.
- DAC revision `25454d8`. E2B Cloud template `base`, sandbox lifetime 15 minutes.

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

| Scenario | Result | Cancel to terminated |
|---|---|---|
| Active command | PASS, 58.02s including the restart check | 8.1s |
| Environment adapter down, then back | PASS, 30.47s | 14.3s |
| Control plane restarted with the stop pending | PASS, 35.02s | 18.8s |
| Runtime adapter down, then back | PASS, 28.15s | 12.0s |

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

## Limits

- One passing run on one host. Nothing here is an availability, cost or production claim.
- Only one command per thread was exercised, and only a plain `sleep` that holds no descriptors.
- `thread/backgroundTerminals/clean` is reported complete by its RPC reply. The process list check, not
  the reply, is the evidence.
- A command that outlives a turn which completed naturally is not cleaned unless a cancellation arrives.
- The failure paths for a lost stop, a lost interrupt, an uncertain start and a rejected start are proved
  by deterministic tests, not against the live providers.
- Stale observations are proved by integration tests. Restarting a real adapter replays none.
