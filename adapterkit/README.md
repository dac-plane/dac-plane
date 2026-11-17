# dac-plane adapter kit

Build a provider adapter for [dac-plane](https://github.com/dac-plane/dac-plane) in Go. The kit dials
dac-plane, negotiates the ControlFrame protocol, keeps the connection alive and hands your code each request.

```sh
go get github.com/dac-plane/dac-plane/adapterkit@v0.3.0
```

Implement `adapterkit.Handler` and call `adapterkit.Run`:

```go
err := adapterkit.Run(ctx, adapterkit.Config{
	URL:      "wss://dac-plane.example:9443",
	Token:    adapterkit.FileToken("/run/secrets/adapter-token"),
	Provider: providerID,
	Role:     wire.RoleArtifactStore,
	Handler:  handler,
})
```

[`examples/minimal`](examples/minimal) is a complete adapter that passes the conformance suite.
`adapterkit.ClientCredentials` is the other token source. Read the package documentation for what a `Handler` must
guarantee: the operation identifier is its idempotency key, and it may offer `wire.CapabilityOperationReplay` only if
it can answer a redelivery from what it already did, across a process failure. A runtime adapter may offer
`wire.CapabilityAgentInstructions` when it places a delegated task's `agent` role and instructions where its runtime
takes standing instructions; dac-plane sends the field only to a connection that selected it.

## Conformance

Test a Go adapter with `conformance.Run`. For an adapter in any other language:

```sh
go run github.com/dac-plane/dac-plane/adapterkit/cmd/adapter-conformance@v0.3.0 -fixture examples/minimal/fixture.json -- ./my-adapter
```

## Versions

The kit is versioned separately from dac-plane, with tags named `adapterkit/vX.Y.Z`. While it is v0 it speaks
ControlFrame protocol 1, and a minor version may change its Go API
([ADR-0018](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0018-release-versioning-and-gate.md)). Each
release's notes name the dac-plane versions it works with.

## Connector role

The `connector` role owns external credentials and offers `list_tools`,
`begin_authorization`, `complete_authorization`, `revoke_connection`, `call_tool`
and `read_call`. The first four requests carry no attempt. Completion requires a
short-lived `confirmation` proof delivered only to the callback browser, which the
owner-authenticated surface passes back. Polling must not disclose this proof.

`call_tool` carries exact authorized input, connection and grant provenance.
`read_call` is an attempt-bound lookup of the same operation's durable result and
must never start or retry an external effect. Unknown outcomes remain unknown.
Revocation is idempotent by connection and must report accepted only after remote
revocation and local credential destruction are complete. A pending reply does
not confirm removal.

Input and output digests cover compact JSON with the HTML escaping performed by
Go's `encoding/json`. Output is bounded to 32 KiB and a catalogue to 48 KiB, leaving
room in the existing 64 KiB payload budget. Credentials never belong in replies,
progress, events or artifacts. See the [connector guide](../docs/guides/use/connectors.md)
for the runtime gateway and a reference implementation.
