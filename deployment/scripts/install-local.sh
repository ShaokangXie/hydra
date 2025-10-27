#!/bin/bash

. vars.sh

echo "Installing Ubuntu packages."

sudo add-apt-repository -y ppa:longsleep/golang-backports

sudo apt-get -y update
sudo apt-get -y install \
	protobuf-compiler \
	protobuf-compiler-grpc \
	git \
	openssl \
	jq \
	graphviz

cd ~


export GOROOT=/usr/local/go
export GOCACHE=~/.cache/go-build
export GIT_SSL_NO_VERIFY=1
export GO111MODULE=on
export PATH=/usr/local/go/bin:$GOPATH/bin:$PATH
export PATH="$PATH:$(go env GOPATH)/bin"

cat << EOF >> ~/.bashrc
export GOROOT=/usr/local/go
export GOCACHE=~/.cache/go-build
export GIT_SSL_NO_VERIFY=1
export GO111MODULE=on
export PATH=/usr/local/go/bin:$GOPATH/bin:$PATH
export PATH="$PATH:$(go env GOPATH)/bin"
EOF

# echo "Installing golang packages. (May take a long time without producing output.)"

echo "Installing gRPC for Go."
go install google.golang.org/grpc@latest

echo "Installing Protobufs for Go."
go install github.com/golang/protobuf/protoc-gen-go

echo "Installing Zerolog for Go."
go install github.com/rs/zerolog/log@latest

echo "Installing Linux Goprocinfo for Go"
go install github.com/c9s/goprocinfo/linux@latest

echo "Installing Kyber for Go"
go install go.dedis.ch/kyber@latest
go install go.dedis.ch/fixbuf@latest
go install golang.org/x/crypto/blake2b@latest

echo "Installing the YAML parser for Go"
go install gopkg.in/yaml.v2@latest

go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

go mod init
go mod tidy
echo "Installation complete."
