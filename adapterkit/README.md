# dac-plane adapter kit

Build a provider adapter for [dac-plane](https://github.com/dac-plane/dac-plane) in Go. The kit dials the control
plane, negotiates the ControlFrame protocol, keeps the connection alive and hands your code each request.

```sh
go get github.com/dac-plane/dac-plane/adapterkit@v0.1.1
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
it can answer a redelivery from what it already did, across a process failure.

## Conformance

Test a Go adapter with `conformance.Run`. For an adapter in any other language:

```sh
go run github.com/dac-plane/dac-plane/adapterkit/cmd/adapter-conformance@v0.1.1 -fixture examples/minimal/fixture.json -- ./my-adapter
```

## Versions

The kit is versioned separately from the control plane, with tags named `adapterkit/vX.Y.Z`. While it is v0 it speaks
ControlFrame protocol 1, and a minor version may change its Go API
([ADR-0018](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0018-release-versioning-and-gate.md)). Each
release's notes name the control-plane versions it works with.
