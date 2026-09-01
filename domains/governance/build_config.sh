#!/bin/bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
set -e
export GOPROXY="https://goproxy.io,direct"
export GOSUMDB=off
cd "$(dirname "$0")/src"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 /opt/homebrew/bin/go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o ../dist/bootstrap ./cmd/config-api
cd ../dist
zip -j config-api.zip bootstrap
rm -f bootstrap
echo "config-api.zip built successfully"
ls -la config-api.zip
