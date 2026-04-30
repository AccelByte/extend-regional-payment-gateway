#!/bin/bash

set -eou pipefail

find_all_proto_files() {
  find "${PROTO_DIR}" -name "*.proto" -type f \
    ! -path "*/google/*" \
    ! -path "*/protoc-gen-openapiv2/*" \
    ! -name "service.proto"
}

PROTO_DIR="${1:-pkg/proto}"
OUT_DIR="${2:-pkg/pb}"
APIDOCS_DIR="${3:-gateway/apidocs}"

# Clean previously generated files.
rm -rf "${OUT_DIR:?}"/* && \
  mkdir -p "${OUT_DIR:?}"

# Clean previously generated swagger files.
rm -rf "${APIDOCS_DIR:?}"/* && \
  mkdir -p "${APIDOCS_DIR}"

# Step 1: Generate Go code for ALL proto files
protoc \
  -I "${PROTO_DIR}" \
  --go_out="${OUT_DIR}" \
  --go_opt=paths=source_relative \
  --go-grpc_out="${OUT_DIR}" \
  --go-grpc_opt=paths=source_relative,require_unimplemented_servers=false \
  --grpc-gateway_out=logtostderr=true:"${OUT_DIR}" \
  --grpc-gateway_opt=paths=source_relative \
  $(find_all_proto_files)

# Step 2: Generate OpenAPI/Swagger for payment.proto (the one with HTTP endpoints)
protoc \
  -I "${PROTO_DIR}" \
  --openapiv2_out "${APIDOCS_DIR}" \
  --openapiv2_opt=logtostderr=true \
  "${PROTO_DIR}"/payment.proto

