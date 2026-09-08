# internal/otlpproto

The OTLP message definitions kitbash compiles in, PLAN.md section 2.7. Nothing
here is written by hand.

## Origin

| | |
|---|---|
| Module | `go.opentelemetry.io/proto/otlp` |
| Version | `v1.11.0` |
| Upstream | <https://github.com/open-telemetry/opentelemetry-proto-go>, tag `otlp/v1.11.0` |
| License | Apache 2.0, kept verbatim in `LICENSE` |

## Why a copy

The upstream packages ship the generated messages next to a gRPC service stub
and a grpc-gateway handler. Importing `collector/trace/v1` for the one request
message linked `google.golang.org/grpc`, `google.golang.org/genproto/*` and
`github.com/grpc-ecosystem/grpc-gateway/v2` into both binaries: 70 packages
neither of them can reach, since kitbashd and kitbash-mcp speak OTLP over HTTP
on a unix socket and never make an RPC.

Taking the messages alone is the option PLAN.md section 2.7 records. The
protocol is still the OpenTelemetry one, byte for byte, because these are the
upstream generated types.

## Files taken

Only the `*.pb.go` message files, never `*_grpc.pb.go` or `*.pb.gw.go`:

- `common/v1/common.pb.go`
- `resource/v1/resource.pb.go`
- `trace/v1/trace.pb.go`
- `logs/v1/logs.pb.go`
- `metrics/v1/metrics.pb.go`
- `collector/trace/v1/trace_service.pb.go`
- `collector/logs/v1/logs_service.pb.go`
- `collector/metrics/v1/metrics_service.pb.go`

The only edits are the import path rewrite from `go.opentelemetry.io/proto/otlp/`
to `github.com/zyx1121/kitbash/internal/otlpproto/`, the two line banner at the
top of each file, and `gofmt`, which the upstream files predate.

## Reproducing the copy

```sh
go generate ./internal/otlpproto/...   # or: internal/otlpproto/vendor.sh v1.11.0
```

The script refuses any file that still reaches the gRPC stack. To move to a
newer upstream, pass the version, rerun the tests, and check that
`go list -deps ./cmd/... | grep grpc` stays empty.
