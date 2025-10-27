#! /bin/bash
# This compiles the protocol buffer files.
# Needs to be run every time any of the *.proto files changes.
# Always run from the project parent directory.

protoc -I protobufs \
  --go_out=paths=source_relative:protobufs \
  --go-grpc_out=paths=source_relative:protobufs \
  protobufs/*.proto