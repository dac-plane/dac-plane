module github.com/dac-plane/dac-plane/adapters/codex

go 1.26.0

toolchain go1.26.10

require (
	github.com/coder/websocket v1.8.15
	github.com/dac-plane/dac-plane/adapterkit v0.1.0
	go.etcd.io/bbolt v1.5.0
)

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/dac-plane/dac-plane/adapterkit => ../../adapterkit
