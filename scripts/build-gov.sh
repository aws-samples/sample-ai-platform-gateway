#!/bin/bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
set -e
# Resolve paths relative to this script so it works from any checkout location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/../domains/governance/src"
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
export GOOS=linux
export GOARCH=arm64
echo "=== Building config-api ==="
go build -o ../dist/bootstrap ./cmd/config-api
echo "=== Creating zip ==="
cd ../dist
rm -f config-api.zip
zip config-api.zip bootstrap
ls -la config-api.zip
echo "=== BUILD DONE ==="
