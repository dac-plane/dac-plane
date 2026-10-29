# Issue #91 — Qualify the first production runtime

Tracker: https://github.com/dac-plane/dac-plane/issues/91
Branch: `feature/91-codex-runtime-qualification`, from merged #99 at `1e0c86b`.
Parent: #75. Depends on #72, #73 and #99. The cancellation gate exposed missing task-environment
orchestration, tracked by [#102](https://github.com/dac-plane/dac-plane/issues/102). Joint deployed
qualification with the #92 environment provider is tracked by
[#101](https://github.com/dac-plane/dac-plane/issues/101); #101 does not require #91 to be closed.

## Direction

Qualify pinned Codex app-server 0.165.0 through a separate out-of-process Go adapter. DAC remains
provider-neutral: Camer routing, model choice, credentials, workspace selection and Codex state stay
inside adapter configuration and deployment. A bounded real app-server spike must prove the native
protocol before the adapter makes any capability claim.

Stable operation replay is advertised only if crash-window tests prove that a lost reply or adapter
restart cannot create a second native turn. A provider availability or credential failure is recorded
as an environmental gate and does not reject Codex.

## Implementation

1. **Candidate gate**
   - [x] Pin and checksum Codex app-server 0.165.0.
   - [x] Execute the bounded native protocol spike: routing, thread start/read/resume, events,
     approval response, interrupt and history reconstruction.
   - [x] Record the observed capability and recovery boundary before adapter completion.
2. **Adapter boundary**
   - [x] Add the independent `adapters/codex` module and `dac-codex-adapter` executable using only
     the published adapter kit.
   - [x] Supervise app-server over stdio with isolated persistent Codex home and file-mounted secrets.
   - [x] Persist DAC operation mappings, native identities, sequences, decisions and replayable results
     in pinned bbolt state.
3. **Runtime translation**
   - [x] Map start, progress, attention, decisions, cancellation, usage, stop and result evidence into
     ordered ControlFrame entries.
   - [x] Reconstruct native history and adapter journal state after reconnect without duplicate event
     identities.
   - [x] Export exact changed fixture artifacts through the deterministic artifact-store fixture.
4. **Qualification and delivery**
   - [x] Add deterministic fake-app-server tests for protocol failures, recovery, crash boundaries,
     redaction and replay claims.
   - [x] Add `just test-codex-adapter` and secret-backed `just qualify-codex-runtime` recipes.
   - [x] Record observed versions, local image identifier, model route, effects, artifacts, durations,
     failures and current provider disposition.
   - [x] Keep this PR's live cancellation evidence scoped to the Codex runtime interruption and
     `runtime_stopped` observation. Do not include the incomplete task-cancel test that waits for
     environment stop evidence.
   - [ ] In a later PR, complete #102 task-environment orchestration and then run #101 with the
     independently qualified #92 provider and this runtime adapter to prove task cancellation
     convergence. #92 owns environment-provider and isolation qualification; #102 owns the DAC
     orchestration path.
   - [x] Run `just ci-fast` and review the final diff; `git diff --check` passed.
   - [ ] Pass the full parallel PR workflow, including Codex image reproducibility, security-race,
     deployed stack and operations jobs.
   - [ ] Keep #91 open until #101 proves cross-adapter cancellation and every other required #91
     acceptance check passes. #101 depends on #92 and #102, not on #91 being closed.

## Limits

This issue does not qualify workspace isolation, egress control or environment ownership (#92), the
production artifact store (#93), user-selected models or BYOK (#94), or the external example surface
(#97). No availability, throughput, cost or provider-wide production claim follows from the bounded
qualification.

## Verification

```text
just test-codex-adapter
just test-conformance
just qualify-codex-runtime
just ci
git diff --check
```
