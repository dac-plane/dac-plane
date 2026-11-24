module github.com/dac-plane/dac-plane/adapters/s3

go 1.26.0

toolchain go1.27.4

require (
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/credentials v1.21.3
	github.com/aws/aws-sdk-go-v2/service/s3 v1.116.0
	github.com/aws/smithy-go v1.28.9
	github.com/dac-plane/dac-plane/adapterkit v0.6.0 // x-release-please-version
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.12 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	golang.org/x/oauth2 v0.38.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/dac-plane/dac-plane/adapterkit => ../../adapterkit
