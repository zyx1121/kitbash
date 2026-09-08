#!/bin/sh
# Copy the OTLP message definitions out of go.opentelemetry.io/proto/otlp into
# internal/otlpproto, rewriting the import paths. Only the *.pb.go message
# files are taken: the *_grpc.pb.go stubs and the *.pb.gw.go gateway would link
# grpc, grpc-gateway and genproto into two binaries that never make an RPC, see
# PLAN.md section 2.7.
# usage: internal/otlpproto/vendor.sh [version]
set -eu

version=${1:-v1.11.0}
module=go.opentelemetry.io/proto/otlp
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
old="$module/"
new="github.com/zyx1121/kitbash/internal/otlpproto/"

# The module is not a dependency of this repo any more, so the version is
# spelled on the argument and go mod download runs outside a module.
src=$(cd "$(mktemp -d)" && go mod download -json "$module@$version" |
	sed -n 's/^	"Dir": "\(.*\)",$/\1/p')
[ -d "$src" ] || { echo "no $module@$version in the module cache" >&2; exit 1; }

files="common/v1/common.pb.go
resource/v1/resource.pb.go
trace/v1/trace.pb.go
logs/v1/logs.pb.go
metrics/v1/metrics.pb.go
collector/trace/v1/trace_service.pb.go
collector/logs/v1/logs_service.pb.go
collector/metrics/v1/metrics_service.pb.go"

# A file the list no longer names is a file upstream dropped or renamed, and
# leaving it behind would keep compiling long after it stopped being generated.
find "$here" -name '*.pb.go' -delete

for file in $files; do
	mkdir -p "$here/$(dirname "$file")"
	{
		echo "// Vendored from $module $version by internal/otlpproto/vendor.sh."
		echo "// Rewritten import paths and gofmt are the only edits."
		echo
		sed "s|$old|$new|g" "$src/$file"
	} > "$here/$file"
	if grep -q 'google.golang.org/grpc\|grpc-ecosystem\|genproto' "$here/$file"; then
		echo "$file reaches the gRPC stack; strip it down to the messages" >&2
		exit 1
	fi
done
# install, not cp: the module cache is read only and cp keeps its mode.
install -m 0644 "$src/LICENSE" "$here/LICENSE"
# The upstream files predate the comment formatting of Go 1.19, and this repo
# lints the whole tree with gofmt -l.
gofmt -w "$here"
