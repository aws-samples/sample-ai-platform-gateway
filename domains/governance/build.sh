#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Build das Lambdas Go do domínio Governance (arm64/Graviton, provided.al2023).
set -euo pipefail
cd "$(dirname "$0")"

export GOPROXY="https://goproxy.io,direct"
export GOSUMDB=off

SRC=src
DIST=dist
mkdir -p "$DIST"

pushd "$SRC" >/dev/null
go mod tidy
popd >/dev/null

build_one() {
  local name="$1"
  echo "building $name..."
  (cd "$SRC" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
    go build -tags lambda.norpc -trimpath -ldflags="-s -w" \
    -o "../$DIST/bootstrap" "./cmd/$name")
  (cd "$DIST" && zip -j "$name.zip" bootstrap && rm -f bootstrap)
}

build_one config-api
build_one pretoken
echo "done."
