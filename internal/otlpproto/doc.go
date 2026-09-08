// Package otlpproto holds the OpenTelemetry protocol message definitions
// kitbash compiles in. The subdirectories are the generated Go types of
// opentelemetry-proto, copied from go.opentelemetry.io/proto/otlp with their
// import paths rewritten and without the gRPC service stubs, because neither
// kitbashd nor kitbash-mcp ever makes an RPC, see PLAN.md section 2.7.
//
// Nothing here is written by hand. vendor.sh reproduces the copy and README.md
// records the version and the exact files taken.
package otlpproto

//go:generate ./vendor.sh
