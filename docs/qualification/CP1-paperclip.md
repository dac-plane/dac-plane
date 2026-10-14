# CP1: Paperclip control-plane candidate

- Date: 2026-10-13
- Result: **Officially supported by Paperclip docs, but FAIL in both tested execution paths**
- Upstream component / version: `paperclipai@2026.1012.0` from npm; GitHub repository `paperclipai/paperclip`; license reported by npm metadata as MIT.
- Host prerequisites: macOS local host, Node.js `24.21.0` via `mise`, Paperclip embedded PostgreSQL, loopback server at `127.0.0.1:3100`.
- Configuration: isolated `PAPERCLIP_HOME` under `/tmp`, Codex local adapter, model `glm-5.3-flash`, Camer Digital base URL intended as `https://api.ai.camer.digital/v1`, credential sourced from `~/.opencodex/config.json` into a temporary shell env var only.
- Decision consequence: do not adopt stock Paperclip as the product control-plane foundation for this phase. Paperclip documents a custom Codex provider path, and its installed adapter contains code for it, but both executed delegated runs still used OpenAI. The layer is therefore promising for lifecycle/state, but the tested release/path fails the provider-neutral gate. Keep it on watch; do not discard it permanently.

## What was tested

The test checked whether Paperclip can occupy the product control-plane layer without taking ownership of the provider/runtime boundary. The required minimum was:

1. install/start a pinned Paperclip version;
2. create a durable work item assigned to a Codex agent;
3. run the agent through the Camer Digital/OpenCodex route, not the default OpenAI endpoint;
4. inspect issue/run/comment evidence after the run.

This corresponds to the layer model used by the project:

```text
Product control plane
  task / attempt / attention / decision / artifact / audit / result state

Runtime adapter
  Codex local adapter

Provider route
  Camer Digital OpenAI-compatible endpoint
```

Paperclip must be judged at that boundary. A local server startup is not enough.

## Official custom-endpoint support check

Paperclip's official Codex adapter docs say custom Codex endpoints are supported through
`PAPERCLIP_CODEX_PROVIDERS`, not through a simple Codex CLI flag. The documented model is:

```json
{
  "providers": {
    "my-gateway": {
      "name": "My gateway",
      "base_url": "https://gateway.example/v1",
      "env_key": "OPENAI_API_KEY",
      "wire_api": "responses"
    }
  },
  "model_provider": "my-gateway"
}
```

The docs state that Paperclip writes this into the managed per-company Codex home's `config.toml`
as `[model_providers.<id>]` tables and selects the top-level `model_provider`. The installed
`@paperclipai/adapter-codex-local` package contains matching code: it parses
`PAPERCLIP_CODEX_PROVIDERS`, writes the managed provider block into `config.toml`, and emits a log
message beginning `Merged ... custom Codex model provider(s) from PAPERCLIP_CODEX_PROVIDERS`.

Therefore the question is not "does Paperclip claim support for custom endpoints?" It does. The
question is "does the tested Paperclip execution path actually apply that support to the delegated
Codex run?" In this run, it did not.

## Upstream status checked on 2026-10-14

The upstream evidence is mixed. It supports a retest later, but it does not overturn the failed
local qualification result.

| Upstream item | State checked | Meaning for this project |
|---------------|---------------|--------------------------|
| [`paperclipai/paperclip#7919`](https://github.com/paperclipai/paperclip/pull/7919) | **MERGED** on 2026-06-11 | This is the real feature source for `PAPERCLIP_CODEX_PROVIDERS`. It claims Paperclip can merge a custom Codex provider into managed `CODEX_HOME/config.toml` and route runtime requests through an OpenAI-compatible `/v1/responses` gateway. |
| [`paperclipai/paperclip#13083`](https://github.com/paperclipai/paperclip/issues/13083) | **OPEN** | Reports that Codex model discovery still calls `api.openai.com` even when runtime gateway routing works. This is adjacent, but not the same as our failure: our delegated runtime call also went to `api.openai.com`. |
| [`paperclipai/paperclip#13086`](https://github.com/paperclipai/paperclip/pull/13086) | **OPEN**, not merged | Fixes model discovery so the UI/model picker uses the custom gateway. The PR explicitly says the runtime Codex HTTP path is unchanged, so this alone is not enough to fix our failed CP2 gate. |
| [`paperclipai/paperclip#11442`](https://github.com/paperclipai/paperclip/issues/11442) | **OPEN** | Confirms `PAPERCLIP_CODEX_PROVIDERS` is expected to select a provider at runtime and that missing `env_key` values produce unclear Codex errors. Our failure is different: the provider block was never merged into managed `config.toml`. |
| [`paperclipai/paperclip#13395`](https://github.com/paperclipai/paperclip/pull/13395) | **OPEN**, not merged | Adds agent-scoped model discovery/gateway catalog plumbing, mainly around Claude gateway discovery. Useful product direction, but not evidence that the released Codex runtime path now works. |
| [`paperclipai/paperclip#14016`](https://github.com/paperclipai/paperclip/pull/14016) | **CLOSED**, not merged | Proposed the cleaner architecture: a managed AI connection carries the gateway endpoint and gives `codex_local` a private `config.toml`. Its manual test says Codex requests went to `<gateway>/v1/responses`, but the PR is not available in the tested release. |

The important conclusion is narrow: Paperclip has a real upstream design for custom Codex gateway
routing, and there are active adjacent fixes. The tested `paperclipai@2026.1012.0` path still failed
to apply that design to our delegated runtime.

Retest Paperclip only when at least one of these changes is true:

1. a Paperclip release after `2026.1012.0` explicitly fixes `PAPERCLIP_CODEX_PROVIDERS` propagation
   into the managed Codex runtime; or
2. an upstream PR merges a managed gateway/AI-connection path for `codex_local` and states that runtime
   requests go to the configured `/v1/responses` endpoint; or
3. this repository carries a local Paperclip patch that proves the provider JSON reaches
   `prepareCodexRuntimeConfig`.

The retest must record all of this evidence before Paperclip can move from **FAIL / WATCH** to a
candidate:

- run log contains Paperclip's merge note for `camer-digital`;
- managed `config.toml` contains `model_provider = "camer-digital"`;
- managed `config.toml` contains a `[model_providers.camer-digital]` table with
  `base_url = "https://api.ai.camer.digital/v1"` and `wire_api = "responses"`;
- no runtime failure/success path mentions `https://api.openai.com`;
- a gateway trace, proxy trace or failure URL proves the request reached `https://api.ai.camer.digital/v1`
  or an equivalent configured test gateway;
- the delegated issue/run produces a durable comment or artifact after the provider route is active.

## Commands and setup

The runnable path used Node 24 because Paperclip requires a newer Node runtime than the active local shell.

```sh
mise x node@24.21.0 -- node -v
```

Observed:

```text
v24.21.0
```

The first controlled run used an isolated data directory and the documented provider shape as a shell env var:

```sh
PAPERCLIP_HOME="$RUN_DIR/data" \
PAPERCLIP_CODEX_PROVIDERS='{"providers":{"camer-digital":{"name":"Camer Digital","base_url":"https://api.ai.camer.digital/v1","env_key":"OPENAI_API_KEY","wire_api":"responses"}},"model_provider":"camer-digital"}' \
mise x node@24.21.0 -- npx --yes --registry https://registry.npmjs.org paperclipai@2026.1012.0 test-drive \
  --data-dir "$RUN_DIR/data" \
  --company-name "DACP CP1 Paperclip" \
  --agent-name "CP1 Codex Agent" \
  --harness codex \
  --model "glm-5.3-flash" \
  --api-key-env CAMER_DIGITAL_API_KEY \
  --no-browser
```

Paperclip `test-drive` strips `PAPERCLIP_*` variables during its isolation step, so this did not reach the adapter.

A second controlled run placed only provider metadata in the isolated instance `.env` before startup:

```text
/tmp/dacp-paperclip-run3.gxJZSz/data/instances/default/.env
PAPERCLIP_CODEX_PROVIDERS={"providers":{"camer-digital":{"name":"Camer Digital","base_url":"https://api.ai.camer.digital/v1","env_key":"OPENAI_API_KEY","wire_api":"responses"}},"model_provider":"camer-digital"}
```

The API key was not written to this file. It was read from `~/.opencodex/config.json` into `CAMER_DIGITAL_API_KEY` for the process and then stored in Paperclip as the agent's `OPENAI_API_KEY` user secret.

The issue was created through the Paperclip API:

```sh
curl -sS -X POST http://127.0.0.1:3100/api/companies/7b195480-b2d7-42c5-ba64-e9dbc9892a34/issues \
  -H 'Content-Type: application/json' \
  --data '{"title":"CP1 provider routing smoke","description":"Read README.md and docs/qualification/control-plane-candidates.md. Add one short comment to this issue saying whether this Paperclip run reached the custom Camer Digital provider path. Do not modify files.","assigneeAgentId":"5660e558-ac70-4c3c-9d39-20ce2feeb713"}'
```

Observed issue:

```json
{
  "id": "e801ced9-5d2c-4871-9bde-919315d8c819",
  "identifier": "DAC-1",
  "title": "CP1 provider routing smoke",
  "status": "todo",
  "assigneeAgentId": "5660e558-ac70-4c3c-9d39-20ce2feeb713"
}
```

The agent was woken through:

```sh
curl -sS -X POST http://127.0.0.1:3100/api/agents/5660e558-ac70-4c3c-9d39-20ce2feeb713/heartbeat/invoke \
  -H 'Content-Type: application/json' \
  --data '{}'
```

Observed heartbeat run:

```json
{
  "id": "2b749220-25b4-4954-b02c-b6190dee8275",
  "status": "queued",
  "invocationSource": "on_demand",
  "triggerDetail": "manual"
}
```

## Evidence

### Test A: `test-drive`

Paperclip installed and started through `test-drive`:

- isolated data dir: `/tmp/dacp-paperclip-run3.gxJZSz/data`
- server: `http://127.0.0.1:3100`
- embedded PostgreSQL port: `54329`
- migrations applied: `304`
- company: `7b195480-b2d7-42c5-ba64-e9dbc9892a34`
- agent: `5660e558-ac70-4c3c-9d39-20ce2feeb713`

The agent config was:

```json
{
  "adapterType": "codex_local",
  "adapterConfig": {
    "model": "glm-5.3-flash",
    "dangerouslyBypassApprovalsAndSandbox": true,
    "env": {
      "OPENAI_API_KEY": {
        "type": "user_secret_ref",
        "key": "OPENAI_API_KEY",
        "version": "latest",
        "required": true
      }
    }
  }
}
```

The work item moved to `in_progress`, and Paperclip wrote a failure comment:

```json
{
  "id": "f41e78e7-ef74-4079-955a-7d299c8c8d88",
  "body": "ACP agent reported a terminal service failure.",
  "createdAt": "2026-10-13T16:45:40.029Z"
}
```

The run log proves the actual provider route was still OpenAI:

```text
unexpected status 401 Unauthorized: Your authentication token is not from a valid issuer.,
url: https://api.openai.com/v1/responses,
auth error code: invalid_issuer
```

No run log contained Paperclip's expected adapter note:

```text
Merged ... custom Codex model provider(s) from PAPERCLIP_CODEX_PROVIDERS
```

Local package inspection found that Paperclip's Codex adapter does contain support for `PAPERCLIP_CODEX_PROVIDERS`, but the tested `test-drive` bootstrap/runtime boundary did not apply that provider config to the actual delegated Codex run.

### Test B: normal `paperclipai run`

A second test used normal Paperclip startup instead of `test-drive`:

```sh
PAPERCLIP_HOME="/tmp/dacp-paperclip-normal.28Q9PG/data" \
paperclipai@2026.1012.0 run \
  --data-dir "/tmp/dacp-paperclip-normal.28Q9PG/data" \
  --bind loopback \
  --force
```

The instance `.env` contained the documented provider config:

```text
PAPERCLIP_CODEX_PROVIDERS={"providers":{"camer-digital":{"name":"Camer Digital","base_url":"https://api.ai.camer.digital/v1","env_key":"OPENAI_API_KEY","wire_api":"responses"}},"model_provider":"camer-digital"}
```

The Paperclip API objects were:

- company: `f4069b90-cb38-4502-ba07-fab0e6efdf4d`
- agent: `909cca5c-72b0-4fad-bae5-2cc8bf1b9154`
- issue: `fb96c411-de1d-4ba1-b1ae-9a1975d33b14` / `DAC-1`
- heartbeat run: `7a651935-d3a7-452a-84ab-44c092010bad`

The agent was created manually through the API with:

```json
{
  "adapterType": "codex_local",
  "adapterConfig": {
    "model": "glm-5.3-flash",
    "dangerouslyBypassApprovalsAndSandbox": true,
    "env": {
      "OPENAI_API_KEY": {
        "type": "user_secret_ref",
        "key": "OPENAI_API_KEY",
        "version": "latest",
        "required": true
      },
      "PAPERCLIP_CODEX_PROVIDERS": {
        "type": "plain",
        "value": "<provider JSON redacted by Paperclip API response>"
      }
    }
  }
}
```

That means the normal-run test put the provider config in both places that should matter:

1. Paperclip host/instance environment;
2. agent adapter env.

The delegated run still failed the same way:

```text
unexpected status 401 Unauthorized: Your authentication token is not from a valid issuer.,
url: https://api.openai.com/v1/responses,
auth error code: invalid_issuer
```

Additional evidence from the normal run:

- no run log contained `Merged ... custom Codex model provider(s) from PAPERCLIP_CODEX_PROVIDERS`;
- the managed Codex home existed at `/tmp/dacp-paperclip-normal.28Q9PG/data/instances/default/companies/f4069b90-cb38-4502-ba07-fab0e6efdf4d/acp-engine/agents/909cca5c-72b0-4fad-bae5-2cc8bf1b9154/codex-home`;
- that managed `config.toml` did not contain `model_provider`, `model_providers`, `camer-digital`, or `https://api.ai.camer.digital/v1`;
- Paperclip posted the issue comment `ACP agent reported a terminal service failure.`

The most likely implementation gap is at the Paperclip adapter boundary. The installed adapter's
provider merge function reads string values from `config.env` before the runtime env is fully resolved.
Paperclip's API stores the explicit agent env as a plain binding object:

```json
{ "type": "plain", "value": "..." }
```

That shape is accepted by Paperclip's API, but it is not a string at the point where
`prepareCodexRuntimeConfig` filters env entries for `PAPERCLIP_CODEX_PROVIDERS`. The process-level
environment path also did not result in the expected merge log or managed `config.toml` provider block.

## Gate results

| Gate | Result | Evidence |
|------|--------|----------|
| CP1 install | **PASS** | `paperclipai@2026.1012.0` started under Node `24.21.0`, created embedded PostgreSQL, applied migrations and served the API/UI on loopback. |
| CP2 provider routing | **FAIL** | Both `test-drive` and normal `paperclipai run` delegated Codex runs called `https://api.openai.com/v1/responses` and failed with `invalid_issuer`; no log proved a call to `https://api.ai.camer.digital/v1`. |
| CP3 delegated task | **PARTIAL** | Paperclip created a durable issue, assigned it to an agent, queued a heartbeat run and moved the issue to `in_progress`; useful agent work did not run because CP2 failed. |
| CP4 approval authority | **NOT RUN** | `test-drive` created the Codex agent with `dangerouslyBypassApprovalsAndSandbox: true`, so this run cannot prove approval blocking/resume semantics. |
| CP5 result evidence | **PARTIAL** | API issue/comment records and file-backed run logs exist; final result/artifact mapping was not reached. |
| CP6 cancellation/cleanup | **PARTIAL** | The local server accepted `SIGINT` and performed a graceful heartbeat drain with no interrupted run IDs; no full worker/session cleanup scenario was tested. |
| CP7 product mapping | **PARTIAL** | Paperclip issue, heartbeat run, comment and run log map to task, attempt and evidence concepts; approval, artifact and result review were not proven. |
| CP8 adapter portability | **PARTIAL** | Paperclip exposes a Codex adapter and source-level custom-provider hook, but the tested bootstrap path did not keep provider routing independent of Paperclip's own harness assumptions. |
| CP9 deployment fit | **PARTIAL** | Self-hosted local startup is plausible, with embedded PostgreSQL and local encrypted secrets; provider routing and approval mode block adoption. |

## Interpretation

This is not evidence that Camer Digital failed. It is also not evidence that Paperclip has no
custom-endpoint feature. It is evidence that the two Paperclip paths we executed did not apply the
documented custom-provider feature to the actual delegated Codex run.

The failure matches the class of problem already disqualifying stock OCC: the control-plane candidate can create state and start a managed runtime, but the actual managed runtime call falls back to the wrong provider endpoint. For this project, that is a primary-failure gate because production agents may not have OpenAI keys and must use the provider/runtime route configured by the product.

Paperclip remains interesting as a source of lifecycle ideas:

- durable issue/task records;
- agent records;
- heartbeat runs;
- retry/escalation behavior;
- comments and file-backed run logs;
- local encrypted secrets;
- adapter concepts.

But it is not adopted. A supported non-OpenAI provider path is a hard gate, and both tested paths
failed that gate.

## Smallest next test

Do not build a Paperclip product integration on the tested release. The next useful work is one of:

1. open an upstream Paperclip issue or local patch proving why `PAPERCLIP_CODEX_PROVIDERS` is not
   reaching `prepareCodexRuntimeConfig` in this run path, despite merged PR #7919; or
2. move to the Kelos candidate test.

For this project, the practical next step is Kelos unless the work item is explicitly "patch and
retest Paperclip." Paperclip can be revisited only if an upstream fix or local patch first produces a
run log containing both:

- Paperclip's provider merge note for `camer-digital`;
- absence of `api.openai.com` in the Codex failure/success path.
