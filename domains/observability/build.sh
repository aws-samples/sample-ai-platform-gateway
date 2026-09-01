#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Compila os Lambdas Go do dominio Observability para Lambda arm64 (Graviton).
set -euo pipefail
cd "$(dirname "$0")/src"
export PATH="/opt/homebrew/bin:$PATH"
export GOPROXY="${GOPROXY:-https://goproxy.io,direct}" GOSUMDB=off
export GOOS=linux GOARCH=arm64 CGO_ENABLED=0

go mod tidy
mkdir -p ../dist
for fn in usage-writer usage-api alert-notifier hints-publisher; do
  echo "building $fn"
  go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o "bootstrap" "./cmd/$fn"
  zip -j "../dist/$fn.zip" bootstrap >/dev/null
  rm -f bootstrap
done
echo "ok: dist/{usage-writer,usage-api,alert-notifier,hints-publisher}.zip"
