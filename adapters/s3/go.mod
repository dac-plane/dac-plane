module github.com/dac-plane/dac-plane/adapters/s3

go 1.26.0

toolchain go1.27.4

require (
	github.com/aws/aws-sdk-go-v2 v1.47.8
	github.com/aws/aws-sdk-go-v2/credentials v1.20.16
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.9
	github.com/aws/smithy-go v1.28.8
	github.com/dac-plane/dac-plane/adapterkit v0.1.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.26 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.11 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.11 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.11 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	golang.org/x/oauth2 v0.38.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/dac-plane/dac-plane/adapterkit => ../../adapterkit
