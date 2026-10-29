module github.com/AssahBismarkabah/dac-plane/adapters/e2b

go 1.26.0

toolchain go1.26.10

require (
	github.com/AssahBismarkabah/dac-plane/adapterkit v0.0.0
	go.etcd.io/bbolt v1.5.0
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/AssahBismarkabah/dac-plane/adapterkit => ../../adapterkit
