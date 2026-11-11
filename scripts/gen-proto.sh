#!/usr/bin/env bash
# Regenerate the versioned internal gRPC/protobuf bindings under api/gen/go.
#
# The generator plugins are locked in go.mod's tool directives, so this target
# is reproducible with the repository's Go toolchain (no globally installed
# protoc plugins required). Only `protoc` itself must be on PATH.
set -euo pipefail

cd "$(dirname "$0")/.."

PROTOC="${PROTOC:-protoc}"
GO_PLUGIN="$(go tool -n protoc-gen-go)"
GRPC_PLUGIN="$(go tool -n protoc-gen-go-grpc)"

includes=(-I api/proto)
for dir in /opt/homebrew/include /usr/local/include /usr/include; do
  if [ -f "${dir}/google/protobuf/timestamp.proto" ]; then
    includes+=("-I${dir}")
    break
  fi
done

rm -rf api/gen/go
mkdir -p api/gen/go

"${PROTOC}" "${includes[@]}" \
  --plugin=protoc-gen-go="${GO_PLUGIN}" \
  --plugin=protoc-gen-go-grpc="${GRPC_PLUGIN}" \
  --go_out=api/gen/go --go_opt=paths=source_relative \
  --go-grpc_out=api/gen/go --go-grpc_opt=paths=source_relative \
  api/proto/scheduler/v1/scheduler.proto \
  api/proto/scheduledruntime/v1/scheduled_runtime.proto

echo "generated:"
find api/gen/go -name '*.go' -print
