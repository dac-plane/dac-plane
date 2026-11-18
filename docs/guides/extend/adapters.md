# Write an adapter

An adapter connects one provider to dac-plane in one role: a `runtime` that runs an agent, an `environment` that
provides the workspace, an `artifact_store` that keeps the output, or an `enforcement` gateway. dac-plane ships
adapters for Codex, E2B and S3-compatible stores; any other provider needs its own. This page covers what an adapter
does, how to build one with the Go adapter kit, how to test it, and how to connect it to a deployment.

## What an adapter does

An adapter is its own process. It opens a TLS WebSocket to dac-plane's adapter endpoint, authenticates with
an OAuth 2.0 access token, and agrees the protocol version. dac-plane never connects to it. Over that
connection dac-plane sends it:

- **requests**: one operation each, such as provisioning a workspace, starting a turn or committing an artifact.
  The adapter performs it against its provider and replies with the outcome;
- **cancels**: a request to stop an operation, confirmed later by what the adapter observes, not by the reply;
- **subscriptions**: a request to stream what the provider reports, from a cursor, so nothing is lost across a
  reconnect.

An adapter reports; it never writes the record. dac-plane decides whether what an adapter observed changes a task,
an attempt or an artifact. The rules the protocol keeps, such as fenced commits, replayable streams and typed
failures, are in [ADR-0003](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0003-provider-contract-semantics.md)
and [ADR-0008](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0008-adapter-process-boundary-and-wire-transport.md).

## Build one in Go

The [adapter kit](https://pkg.go.dev/github.com/dac-plane/dac-plane/adapterkit) handles the connection: dialling,
authentication, version negotiation, keep-alive, and reconnecting with backoff before the token expires.

```sh
go get github.com/dac-plane/dac-plane/adapterkit@v0.4.0
```

Implement `adapterkit.Handler`, with `Send` for requests, `Cancel` for cancels and `Subscribe` for streams, and
call `adapterkit.Run`:

```go
err := adapterkit.Run(ctx, adapterkit.Config{
	URL:      os.Getenv("DAC_ADAPTER_URL"),
	Token:    adapterkit.FileToken(os.Getenv("DAC_ADAPTER_TOKEN_FILE")),
	Provider: os.Getenv("DAC_PROVIDER_ID"),
	Role:     wire.RoleRuntime,
	Handler:  handler,
})
```

Two rules matter more than any other:

- **The operation identifier is the idempotency key.** The same request can arrive more than once, after a reconnect
  or a dac-plane restart. Performing it twice must not do the work twice.
- **An error from `Send` means the outcome is unknown.** The kit reports it that way, and dac-plane reconciles from
  what the adapter later observes. Return an error only when you cannot tell what happened; otherwise reply with the
  outcome, including a definite failure.

An adapter may offer `wire.CapabilityOperationReplay` only if it can answer a repeated delivery from what it already
did, across a process restart. The shipped adapters keep a small journal on disk for this.

A runtime adapter may offer `wire.CapabilityAgentInstructions` to take tasks delegated to an
[agent](../use/agents.md). Its `start_turn` arguments then carry an `agent` object, with a `role` and optional
`instructions`, beside the `brief`. Place them where the runtime takes standing instructions, apart from the brief: the
brief is the owner's request, the instructions are the tenant's. dac-plane never sends the field to a connection that
did not select the capability, so an adapter that decodes its arguments strictly keeps working without it.

An artifact store adapter may offer `wire.CapabilityArtifactDownload` to let surfaces download what it stores
([ADR-0025](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0025-serve-artifacts-to-surfaces-through-the-control-plane.md)).
It is then sent `authorize_download` with an admitted `manifest` and the committed `object_version`, and answers with
a `download`, an HTTPS URL that reads exactly that version, and its `expires_at`, at most fifteen minutes away. Refuse
any other version. dac-plane fetches the URL itself, only from origins the operator lists, so it must reach
the URL's host; it never stores or forwards it. A store that does not offer the capability is never sent the request,
and its artifacts' content answers `503` to surfaces.

A runtime adapter that can ask a person something in words raises an `attention.requested` entry of kind `question`
with a `question` member: 1 to 8 prompts, each with an `id`, optional `header`, the `text` (at most 2048 characters),
and optional `options` (a `label` and a `description`; `other` allows a free-text answer as well). dac-plane serves the
text to the task owner and steers the runtime with the answer: the `steer_turn` arguments of an `answer` decision carry
`answer`, one string per prompt `id`, beside `answer_digest`. A request outside the bounds is rejected, so decline what
cannot be shown whole, and never offer a secret prompt: its answer would be stored
([ADR-0026](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0026-keep-question-and-answer-text-as-product-records.md)).
A request without a `question` member is still valid and is answered by digest alone, so an adapter that sends none keeps
working.

[`examples/minimal`](https://github.com/dac-plane/dac-plane/tree/main/adapterkit/examples/minimal) is a complete
adapter that passes the conformance suite, and the
[Codex](https://github.com/dac-plane/dac-plane/tree/main/adapters/codex),
[E2B](https://github.com/dac-plane/dac-plane/tree/main/adapters/e2b) and
[S3](https://github.com/dac-plane/dac-plane/tree/main/adapters/s3) adapters are full ones.

## Test it

The conformance suite plays dac-plane and checks the protocol: negotiation, requests, cancels, streams and
reconnects. For a Go adapter, call `conformance.Run` from a test. For one in any language, the runner starts your
adapter as a command:

```sh
go run github.com/dac-plane/dac-plane/adapterkit/cmd/adapter-conformance@v0.4.0 \
  -fixture fixture.json -- ./my-adapter
```

It passes the adapter `DAC_ADAPTER_URL`, `DAC_ADAPTER_CA_FILE` and `DAC_ADAPTER_TOKEN_FILE`. The fixture names the
provider, its role and one request the adapter answers; the example's
[`fixture.json`](https://github.com/dac-plane/dac-plane/blob/main/adapterkit/examples/minimal/fixture.json) shows
the shape.

## Connect it to a deployment

1. Register the provider in its role with
   [`provision register-provider`](../reference/cli.md#register-provider). A runtime also names its artifact store.
2. Create a confidential client for the adapter at your identity provider, with the client-credentials grant and a
   token for the adapter endpoint's audience. That issuer must be listed under `adapter_issuers` in the
   [identity configuration](../reference/configuration.md#identity-configuration-file).
3. Bind the client's identity to the registration with
   [`provision bind-adapter`](../reference/cli.md#bind-adapter). Use one client per registration.
4. Run the adapter with the settings every adapter reads, under
   [Every adapter](../reference/configuration.md#every-adapter) in the configuration reference.
5. Check that dac-plane logs `adapter connected` for it.

A runtime also needs a harness qualification before dac-plane sends it work:
[`provision qualify-harness`](../reference/cli.md#qualify-harness).

## Versions

The kit is versioned apart from dac-plane, with tags named `adapterkit/vX.Y.Z`. While it is v0 it speaks
protocol version 1, and a minor version may change its Go API. Each release's notes name the dac-plane versions
it works with.
