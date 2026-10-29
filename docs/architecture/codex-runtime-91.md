# Issue #91 Codex runtime qualification evidence

Date: 2026-10-29. Host architecture: Darwin arm64. Confidence: high for the observed deterministic,
protocol and deployed approval/result behavior; cancellation lifecycle convergence remains unproven.

This note separates the candidate protocol gate, deterministic adapter verification and deployed
qualification. It is not evidence for workspace isolation (#92), a production artifact store (#93),
user-selected credentials or models (#94), or the external demonstration surface (#97).

## Candidate and image pins

The candidate is Codex app-server `0.165.0`, configured with the Responses protocol. The direct
spike used the official Darwin arm64 archive:

```text
archive: codex-app-server-aarch64-apple-darwin.tar.gz
sha256: 6d5530758a51aa67ef1e8a843145f0a6fc14aef8ceab13a6f5e9dc84d55b716c
binary: codex-app-server 0.165.0
model provider: camer
model: deepseek-v4-flash-0731
```

Against a loopback Responses endpoint, the exact binary observed the configured bearer credential,
created a thread, emitted live turn notifications, completed a turn, returned the thread through
`thread/read` with `includeTurns:false`, and resumed the persisted thread. Custom Camer routing and
the basic start/read/resume protocol gate therefore passed without sending a request to Camer.

Two native limitations were observed. `thread/read` with `includeTurns:true` returned
`list_turns is not supported yet`. An empty thread that had never started a turn could not be resumed
because it had no rollout. These observations constrain recovery; they are not papered over by an
adapter capability claim.

The qualification image was built locally with the official Linux arm64 archive checksum
`431e82c7dfa08c2aa97cdd637d09324557c8de8140279184259eefbe871e4e26`. Running the embedded binary
reported `codex-app-server 0.165.0`. The observed local OCI image identifier was:

```text
sha256:d1c8f15025ca38a5b0ff0ae9365293ad4f9dec30e7a48497546724d21555e32b
```

This is a local image identifier, not a registry digest. The amd64 build pins the official checksum
`5f6940dae95bc711b699f57c4edba522b7de682e267901c37a03038c3b440fc0` but was not built on this host.

## Deterministic adapter evidence

`go test -race ./...` in `adapters/codex` passed in 4.823 seconds. The suite exercised strict execution
brief decoding and bounds, operation mapping, changed-payload rejection, the lost-`thread/start`
crash window, restart resume/read, ordered journal identities, approval and denial translation,
prepared-decision retry, interruption, exact artifact hashing and upload, unsafe upload rejection,
credential redaction, malformed native messages and the published ControlFrame conformance suite.
The previous local `just ci` run passed on 2026-10-29 before the CI recipes were aligned with all
parallel PR jobs. After adding the fast local gate and the missing PR jobs, `just ci-fast` passed;
the full PR workflow still needs to run against the pushed branch. `git diff --check` passed after
the evidence update.

The adapter does not advertise `operation.replay`. If `thread/start` succeeds but its reply is lost
before the returned thread identifier is journaled, app-server exposes no stable caller correlation
with which to find that thread. A redelivery is rejected as uncertain rather than risking a second
native turn. Once a mapping is durable, restart reconstruction resumes the native thread and combines
its current state with the adapter journal without assigning new identities to recorded events.
Approval request identifiers are scoped to one supervised app-server process: after restart, a stale
JSON-RPC request identifier is rejected until app-server reissues the exact native request.

## Deployed qualification status

The secret-backed qualification used the user-authorized `CAMER_DIGITAL_API_KEY` environment
credential, written only to a mode-0600 file under `.work/codex-qualification/` and removed after the
run. The credential supplied by the local OpenCode configuration had previously returned HTTP 401;
that credential was not used for the successful inference attempts. The current run emitted no
credential in its logs, and the recipe's credential-leak check passed.

The pinned Linux arm64 Codex app-server reported version `0.165.0`. The local OCI image identifier
for the latest full run was
`sha256:a4b4d189970f8c7af0d0c8e93b89b5efb00f2f9bc7f7e2f92129ccfececb2152`; it is not a registry
digest. The model route was `camer` / `deepseek-v4-flash-0731` through
`https://api.ai.camer.digital/v1`.

Observed successful scenarios:

- The task requested a constrained command, received one-time approval requests, and the adapter
  consumed the approvals. The task reached `ready_for_review`, exported exactly `qualified\n` through
  the deterministic artifact fixture, survived a DAC and adapter restart after result creation, and
  was accepted to `succeeded`. A targeted deployed run completed in 16.261 seconds.
- Two denied-action cases consumed their denial decisions, produced no requested workspace files,
  and were cancelled through the HTTP API. Both reached a terminal task state.
- In the full run, cancellation of an active `sleep 300` command caused Codex to report the
  interrupted turn and DAC to record `runtime_stopped`. The attempt remained
  `cancellation_requested`, with `environment_stopped=false`, and the task did not reach
  `cancelled`. The deployed test timed out after 305.283 seconds while waiting for that lifecycle
  transition.

The full task-cancellation experiment did not pass: the qualification topology has no task-environment
orchestration or environment binding. DAC correctly does not infer environment termination after a
runtime dispatch may have reached the provider. The missing product path is tracked by
[#102](https://github.com/dac-plane/dac-plane/issues/102). The complete task-cancellation scenario is
reserved for [#101](https://github.com/dac-plane/dac-plane/issues/101), after #102 and the independent
environment-provider qualification in [#92](https://github.com/dac-plane/dac-plane/issues/92). That
later PR will test the #91 runtime together with the #92 environment adapter. The current #91 PR keeps
only the observed Codex interruption and `runtime_stopped` evidence; it does not include a test that
waits for task cancellation without the required environment path. Keep #91 open and the provider
unqualified (`off`) until #101 passes. This limitation does not invalidate the observed Codex
`turn/interrupt` behavior.

The deterministic artifact fixture proves adapter export integration only; it does not qualify the
production artifact store owned by #93. The deployed qualification still lacks recovery evidence
across an unresolved native approval. Issue #101 covers the later joint task-environment and runtime
cancellation boundary. Promotion to `shadow` and then `live` requires all mandatory deployed
scenarios, recorded artifact hashes and a registry image digest.
No availability, throughput, cost, RPO, RTO, provider-wide production, environment-isolation or
workspace-containment claim follows from the observations in this note.

## Implementation status

1. **Candidate gate**
   - [x] Pin and checksum Codex app-server 0.165.0.
   - [x] Execute the bounded native protocol spike for custom routing, thread start/read/resume, live
     events, approval response, interruption and history reconstruction.
   - [x] Record observed native limitations before making adapter capability claims.
2. **Adapter boundary and translation**
   - [x] Add the independent `adapters/codex` module and executable using the published adapter kit.
   - [x] Supervise app-server over stdio with persistent isolated home and file-mounted secrets.
   - [x] Journal operation mappings, native identities, decisions and replayable results.
   - [x] Translate runtime events, attention, decisions, cancellation and result metadata into
     ControlFrame; export fixture artifacts through the deterministic store.
   - [x] Keep `operation.replay` unadvertised because the lost `thread/start` reply cannot be recovered
     without risking a duplicate native turn.
3. **Verification**
   - [x] `go test ./...` in `adapters/codex` passed after latest edits.
   - [x] `go test -race ./...` in `adapters/codex` passed after latest edits.
   - [x] Tagged qualification driver compiled with no tests run.
   - [x] Targeted deployed approval/artifact scenario passed in 16.261 seconds.
   - [ ] Complete full task cancellation in #101 after #102 provides task-environment orchestration
     and #92 qualifies the environment provider.
   - [x] `just ci-fast` passed; `git diff --check` passed after the final evidence and scope edits.
   - [ ] Full parallel PR workflow, including reproducible Codex image build, security-race tests,
     deployed stack and operations suite, is pending on the pushed branch.

The full deployed run reached the cancellation case after successful approval/artifact and denial
scenarios. It stopped waiting after 305.283 seconds with `runtime_stopped=true` and
`environment_stopped=false`; this is recorded as an unpassed end-to-end gate, not a waiver. The
incomplete wait has been removed from the current branch's qualification recipe; #101 owns the full
scenario after #102 and #92 are available.
