module github.com/agynio/agynd-cli

go 1.26.1

replace github.com/agynio/claude-sdk-go => github.com/spk-ai/claude-sdk-go v0.2.2-0.20260913223410-16286f344ebc

require (
	github.com/agynio/agn-sdk-go v0.1.0
	github.com/agynio/claude-sdk-go v0.2.1
	github.com/agynio/codex-sdk-go v0.1.1-0.20260324143246-85080838f61f
	github.com/google/uuid v1.6.0
	go.opentelemetry.io/proto/otlp v1.10.0
	google.golang.org/grpc v1.79.2
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
)
