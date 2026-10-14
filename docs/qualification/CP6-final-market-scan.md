# CP6: Final market scan and CanDoItAll runtime qualification

- Source screen: 2026-10-14
- Runtime qualification: 2026-10-14
- Result: **CLOSED — CANDOITALL REJECTED AS THE PRIMARY CONTROL-PLANE FOUNDATION**

## Decision

The earlier CP8 failure was a test-configuration error, not evidence that custom endpoints are lost on
restart. The first run edited CanDoItAll's fixed-ID built-in `OpenAI chat completions` seed. Current
startup code intentionally reconciles that managed seed to the canonical OpenAI URL and model on every
startup. A separately created local provider profile is the supported custom-provider path.

The corrected run created `Camer Digital qualification` through `POST /api/agents/providers`, called
DeepSeek successfully, restarted the application container, read the same profile back unchanged and
called DeepSeek successfully again. CP8 therefore passes for provider-profile persistence. The previous
post-approval `401 invalid_issuer` was caused by our mutation of the managed seed and must not be used to
reject CanDoItAll or Camer Digital.

The process/workflow qualification is now complete. CanDoItAll passes the custom-provider, approval,
restart-reconciliation and cancellation gates. Its workflow external-request records bind the action,
expected version, authorization policy, responder identity, scope fingerprint, response lifetime and
idempotency key. Its process runtime persists task-like runs, step attempts, events, recovery decisions,
result lineage, rework and terminal cancellation. A forced restart during an agent step did not silently
repeat an uncertain side effect: startup recovery blocked the step and required an operator decision.

CanDoItAll nevertheless fails CP7 for this product. Its `BudgetPlan` contains loop-repeat limits, and its
run records aggregate observed token usage and cost, but it has no authoritative product task/attempt
budget record or pre-execution enforcement contract for spend, tokens, elapsed time or resources. That
missing record cannot be supplied by mapping observed cost or a loop counter to the product's `Budget`.
Adopting CanDoItAll as the lifecycle owner would therefore leave the product as a second authority for a
hard part of the same task lifecycle. CP7 is a hard disqualification gate.

CP9 also fails for current adoption: the pinned source has no published release, requires sibling source
repositories, disables hosted CI, starts this Linux deployment with `BasicLocal` secret protection and
unencrypted data-protection keys, and restored packages with known high-severity advisories. These issues
may be repairable, but the project does not need to accept that integration risk after CP7 has already
failed.

Confidence: **high**.

## Pinned inputs

| Repository | Commit | Role |
|------------|--------|------|
| [CanDoItAll](https://github.com/fyziktom/CanDoItAll) | `5d4156a053e35ff295e711db5275428e279697ed` | Application |
| [CanDoItAll.Components](https://github.com/fyziktom/CanDoItAll.Components) | `f258ab6a959a97fa16c01d0858e7dc122728a11a` | Required sibling source |
| [CanDoItAll.FileTools](https://github.com/fyziktom/CanDoItAll.FileTools) | `498b36825bd5a5222429972af120b04becf4b3f6` | Required sibling source |

The repositories are MIT licensed. The test used Docker 29.8.2 with Compose 5.5.0 on an arm64 macOS
host. The application image was built from source; disposable runs were exposed only on loopback ports
`18080` and `18082`. PostgreSQL
18.6 ran on the Compose backend network. The application container used a read-only root filesystem,
`no-new-privileges` and dropped Linux capabilities as supplied by the candidate's Compose definition.

## Non-secret provider configuration

The Camer Digital credential came from the existing `camer-digital` provider entry in
`~/.opencodex/config.json`. The value was passed to Compose through `OPENAI_API_KEY`; it was never written
to this repository or printed in test output. The configured provider profile was:

```text
provider name: Camer Digital qualification
base URL:      https://api.ai.camer.digital/v1
transport:     OpenAI Chat Completions
model:         deepseek-v4-flash-0731
secret ref:    secret:86f781f1-1e76-4b45-9f1a-42b8cf13d8c7
```

`glm-5.3-flash` was also attempted. Its health probe reached Camer Digital but returned `429 Model busy,
retry later`. Switching the same profile to `deepseek-v4-flash-0731` passed three consecutive health
probes. `POST /api/agents/providers/{id}/test-chat` then returned `CP6_PROVIDER_OK` with input and output
token counts. This proves that custom endpoint routing itself works; the failure is not a general Camer
Digital or OpenAI-compatibility problem.

The profile required an explicit model-price row before CanDoItAll allowed an agent to select the custom
model. Because the Camer key's commercial tariff was not part of this test, the disposable profile used
the product's explicit-free tariff marker solely to exercise lifecycle behavior. No financial-budget
claim is derived from the resulting zero cost.

## Runtime evidence

### Control run without restart

The seeded `Programming Workspace Analyst` was restricted to the `workspace_write_file` capability and
configured to use the Camer Digital profile. Run `e6d673a7-2776-4a18-a163-a73930955170` requested a write
to `cp6-control-evidence.txt`.

Observed API state:

1. The model proposed one `workspace_write_file` call.
2. The run entered state `3` (`WaitingOnTool`) with one pending approval.
3. The file did not exist before the decision.
4. Approval `ficc_chatcmpl-tool-a47b033ba748a055` was approved through the execution-run API.
5. The run entered state `5` (`Completed`) with outcome `0`.
6. The API exposed the approval, a `MutatingWorkspace` tool receipt, provider/model metrics, token usage,
   the generated-output artifact and the final response.
7. The file contained exactly `CANDOITALL_CP6_CONTROL_OK`.

This establishes that provider routing, pause-before-effect, approval continuation, artifact capture and
result readback work when the application does not restart.

### Invalid restart run: managed seed was edited

Run `4d464972-b1ea-49df-aa34-8e70a5408bcf` requested a write to `cp6-evidence.txt` and reached the same
pending-approval state. The application container was then restarted before the approval was answered.

After restart:

- the execution run, pending approval and revision survived in PostgreSQL;
- the file was still absent before approval;
- the provider editor had reverted from Camer Digital to `https://api.openai.com/v1`, model
  `gpt-5.4-mini`, name `OpenAI chat completions`, and the custom price row was gone;
- the agent still referenced the same provider profile identifier and custom DeepSeek model;
- approving the persisted request executed the workspace write and created an artifact receipt; and
- continuation then failed terminally with `HTTP 401 (invalid_request_error: invalid_issuer)` because the
  restored profile sent the Camer credential to OpenAI.

The persisted run recorded state `6` (`Failed`), the approved decision, the completed side-effect receipt
and generated artifact. Its log explicitly reported `CompatibleRestore` immediately before invoking the
tool, then reported a provider failure for `OpenAI chat completions`.

This run remains useful failure evidence, but its original interpretation was wrong. Fixed provider ID
`036b360a-e3f4-8350-97ca-f88de60ba2bb` is a runtime bootstrap seed. `ProviderDefaultsBootstrapService`
finds that fixed ID/name and rewrites its URL, model and metadata to the built-in OpenAI defaults during
startup. Editing it is unsupported for a durable custom provider. The resulting post-effect failure does
show why adapters must pin configuration identity, but it does not fail CanDoItAll's custom-provider
portability gate.

### Corrected restart run: separate custom profile

The corrected test used a clean Compose project from the same pinned commits and created a new provider
through the documented API contract (`id: null`, `expectedConcurrencyToken: null`). The server assigned
profile `698c1f89-82a4-420d-b54d-26acab365b40` with the Camer Digital base URL and
`deepseek-v4-flash-0731` model.

Before restart, the provider test-chat returned exactly `CP6_CUSTOM_PROVIDER_OK`. After restarting the
application container and waiting for its health check, the editor returned the same profile identifier,
name, base URL, model, transport and secret reference. A new provider test-chat then returned exactly
`CP6_AFTER_RESTART_OK`, with input and output token usage.

Tavily and GitHub issue/PR searches found no reported CanDoItAll defect matching "custom provider reset
on restart". The absence of an issue is consistent with the source: managed-seed reconciliation is
intentional, while separately created profiles are persisted. The current upstream commit was still the
pinned `5d4156a053e35ff295e711db5275428e279697ed` during this recheck.

### Workflow authority and continuation

The seeded `Example: Approval Gated HTTP Action` workflow was run through the authenticated API. Run
`e3c2aa62-5d73-4ad1-92f9-2b9c6c7dfa07` paused first at its human approval and then at the governed HTTP
executor boundary. The application was recreated while the second request was pending; the run and
request survived. The following controls were observed:

- no bearer token returned `401`, and a valid session token without workflow-response scope returned
  `403`;
- an omitted idempotency key returned `400`, a stale expected version returned `409`, and a schema-invalid
  response returned `400`, without consuming the pending request;
- the accepted operation persisted responder subject `cp6-qualified-operator`, actor kind, scope
  fingerprint, expected request version and authorization-policy fingerprint;
- the request boundary persisted origin actor, executor identity, required capabilities, approval
  requirement and a 900-second response-authorization lifetime;
- exact replay returned the same operation with `replayed: true`, while reusing the key with a changed
  response returned `409`; and
- after the second approval, the run completed with three checkpoints and three artifacts.

A separate denial run, `dcbe0e60-fdea-41a7-bcd9-71888779dcfe`, denied the governed executor request. It
completed without invoking the HTTP executor and without producing its artifact. This proves that the
decision controls the intended side effect rather than merely recording an advisory approval.

### Process run, restart recovery, rework and cancellation

The process test created a separate Camer Digital provider, assigned the seeded Delivery Manager to
`deepseek-v4-flash-0731`, and launched the two-step `project-structure-note-writeback` definition. The
prepared admission and launch used the same caller-intent identifier. Replaying the launch returned the
same run; changing the input under that identifier returned `409`; a token without process-execution
scope returned `403`.

Process run `bed87724-5b03-4f72-ba23-a88b06e2a4ed` persisted its immutable plan, two assigned steps and
the sequence `ProcessRunCreated`, `ProcessRunActivated`, `StepReady`, `StepClaimed`,
`DispatchClaimCreated`, `StepRunning`. The actual agent call went through Camer Digital and invoked
governed tools with the process-run and step identities.

The application container was then forcibly restarted while the first step was running. Startup recovery
retained execution run `0cb69d8a-c71c-4081-8579-1494fb970302`, completed its dispatch claim, blocked the
step with diagnostic `process.adapter.agent_interrupted_execution_replay_unsafe`, and moved the process
to `NeedsAttention`. Its recovery record said the side-effect state was indeterminate, automatic replay
was unsafe and manager review was required. It did not silently repeat the attempt.

An authenticated rework decision changed the step back to `Ready` and the run to `Active`. An immediate
explicit cancel then produced `ProcessRunCancelRequested` and `ProcessRunCancelled`; the run became
terminal `Cancelled`, both unfinished steps were marked cancelled, and no active claim or worker remained.
The terminal summary retained the plan, participant, execution-run identity, attempt count, rework count,
event history, recovery decision and evidence-completeness warnings.

Two earlier direct-run client-abort probes also left no matching persisted execution or active worker,
but the explicit process cancellation above is the authoritative CP6 evidence.

## CP1-CP9 result

| Gate | Result | Evidence |
|------|--------|----------|
| CP1 install | **PASS** | The three pinned repositories built into an arm64 image and the documented Compose stack became healthy. |
| CP2 provider routing | **PASS** | The custom base URL was accepted; DeepSeek passed three health probes and a test chat through Camer Digital. |
| CP3 delegated task | **PASS** | Durable execution-run state, logs, metrics and approvals were queryable and survived application restart. |
| CP4 approval authority | **PASS** | The workflow request blocked the intended executor, bound responder identity/scope, action, version, policy and lifetime, rejected unauthenticated, unauthorized, stale and conflicting responses, and survived restart. |
| CP5 result evidence | **PASS** | API readback exposed outcome, provider/model, token metrics, log entries, approval, tool receipt, artifact metadata and final response. |
| CP6 cancellation/cleanup | **PASS** | Explicit process cancellation persisted request and terminal events, cancelled both unfinished steps and left no active claim or worker. |
| CP7 product mapping | **FAIL** | Runs, attempts, attention, decisions, artifacts, audit, result lineage and review/rework map, but the only native budget is a loop-repeat budget. Observed usage/cost is not an authoritative task/attempt budget or enforcement contract. The product would remain a second lifecycle owner. |
| CP8 adapter portability | **PASS** | A separately created Camer Digital profile retained its ID, endpoint, model, transport and secret reference across restart, and DeepSeek completed calls before and after restart. |
| CP9 deployment fit | **FAIL FOR CURRENT ADOPTION** | There is no published release, hosted CI is disabled, maintenance is concentrated, the tested Linux path used `BasicLocal` secret protection and unencrypted data-protection keys, and the pinned build restored dependencies with known high-severity advisories. |

CP7 is a hard gate, so CanDoItAll is rejected as the primary control-plane foundation. The passing gates
remain useful evidence: its provider adapter, workflow external-request protocol and uncertain-effect
recovery design may be studied or wrapped as narrower replaceable components, but they do not remove the
need for this product to own its complete work contract.

## Product mapping observed

| Required product record | Closest CanDoItAll record | Decision |
|-------------------------|---------------------------|----------|
| Task | Process run | **Usable only if CanDoItAll owns the lifecycle**; direct agent and workflow runs are subordinate executions, not alternate product tasks. |
| Attempt | Step claim plus agent execution run | **Usable**; identities, attempt count, outcome and restart recovery are durable. |
| Attention request | Workflow external request or blocked process recovery | **Usable**; pending state, action/version and policy survived restart. |
| Decision | Workflow external-response operation or process rework/cancel decision | **Usable**; identity, scope, idempotency, stale/conflict handling and effect were observed. |
| Budget | `LoopBudgetPlan` plus observed usage/pricing | **FAIL**; repeat limits and post-hoc cost totals are not an authoritative task/attempt budget or pre-execution enforcement record. |
| Artifact | Execution artifact and process artifact lineage | **Usable**; generated output, producer and process lineage are API-readable. |
| Audit event | Workflow operations, process runtime events, execution logs and tool receipts | **Usable**; restart recovery and operator actions remain in durable history. |
| Result revision | Execution response, result hash and terminal process run record | **Partial**; immutable evidence exists, but no independent product result-revision lifecycle was demonstrated. |
| Result review | Manager-required recovery plus explicit rework | **Usable process review primitive**; the runtime required an operator decision before unsafe replay. |

## Security and maturity observations

The source build completed, but NuGet emitted `NU1903` high-severity vulnerability warnings for:

- `System.Security.Cryptography.Pkcs` 10.0.10 (`GHSA-846f-f764-76mx`); and
- `System.Security.Cryptography.Xml` 10.0.10 (`GHSA-7hqp-7c5m-4vq6`,
  `GHSA-c399-9853-6jmj`, `GHSA-r955-8q5j-3h83`).

The same restore also reported a moderate Pkcs advisory. Application startup warned that ASP.NET data
protection had no XML encryptor and that the local secret vault's encryption key was accessible to the
same operating-system account. These observations do not claim exploitability in every deployment. They
must be resolved or explicitly mitigated before adoption can pass CP9.

At final verification, the GitHub Releases API still returned no published releases and upstream `main`
still resolved to the pinned commit. The repository README also states that GitHub-hosted CI is
temporarily disabled and that package/release tooling has not yet been introduced. CanDoItAll requires
two sibling source repositories, retains public-alpha upgrade notes and has contribution heavily
concentrated in one maintainer. These facts do not erase its demonstrated lifecycle capabilities, but
they fail the operational confidence required for current foundation adoption.

## Other final-scan candidates

| Candidate | Pinned source | Result |
|-----------|---------------|--------|
| [BoringOS](https://github.com/hebbs-ai/boringos) | `ba9ddc8ef6ac87d3e4317c0a1b1ea0fec4260b10` | **REJECTED**: approval, result-review and task/attempt semantics do not meet the hard mapping gate. |
| [OCCP](https://github.com/azar-management-consulting/occp-core) | `c19c74b9a6b41972d3e9be89e13356c6fa646af6` | **REJECTED**: mutable task/result and task-keyed approval records do not supply the required lifecycle ledger. |

## Consequence

Do not adopt CanDoItAll as the primary control-plane foundation. The bounded qualification is closed:
CP4, CP6 and CP8 pass, but CP7 fails the required budget mapping and CP9 fails current deployment fit.

The market scan therefore found no existing candidate that can own the complete product work contract.
Implement the minimum product-owned lifecycle core for task, attempt, attention, decision, budget,
artifact, audit and result/review state. Keep runtime, environment, workflow and policy integrations
behind replaceable adapters. CanDoItAll remains a useful source-level reference for authority-bound
external responses, replay-safe command handling and uncertain-effect restart reconciliation; reusing
those ideas or integrating a narrow component does not make its application state authoritative.
