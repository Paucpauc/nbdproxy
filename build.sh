#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o nbdproxy-linux-amd64 .
