#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# verify-decoupling.sh — CI guard against coupling regressions.
#
# Fails (exit 1) when it finds forbidden literals or architectural anti-patterns in the
# tracked Terraform / Go of the domains. Every check below exists because the pattern it
# looks for was actually committed at some point: a live account id, a hardcoded API
# Gateway URL, cross-domain remote state. Each one couples two domains that are supposed
# to talk only through the SSM Environment Contract.
#
# Scope: domains/**. Ignores .terraform/, dist/ and tools/decoupling (sample fixtures).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
report() { echo "FAIL: $1"; fail=1; }

# TRACKED files only — git ls-files, not find.
#
# This distinction is the difference between a useful guard and a guard everyone learns
# to ignore. `find` also walks gitignored paths: local experiments, provider caches and
# the untracked internal tooling. Those are not published, so a literal in them couples
# nothing, yet the script reported FAIL and exited 1. A CI guard that fails on files that
# will never ship trains people to skip it.
tf_files() {
  git ls-files -- 'domains/**/*.tf' 'domains/**/*.tftpl' 2>/dev/null
}
go_files() {
  git ls-files -- 'domains/**/*.go' 2>/dev/null
}

echo "== no 12-digit account id =="
# Excludes 200601021504: Go's reference time layout, not an account id.
if grep -nE '\b[0-9]{12}\b' $(tf_files) $(go_files) 2>/dev/null | grep -vE '200601021504'; then
  report "12-digit account id found (use data.aws_caller_identity)"
fi

echo "== no live URL or resource id as a literal =="
# execute-api
if grep -nE 'execute-api\.[a-z0-9-]+\.amazonaws\.com' $(tf_files) 2>/dev/null; then
  report "literal execute-api URL (must come from the SSM Contract)"
fi
# cloudfront
if grep -nE '[a-z0-9]+\.cloudfront\.net' $(tf_files) 2>/dev/null; then
  report "literal cloudfront domain (must come from the SSM Contract)"
fi
# cognito pool id (e.g. us-west-2_XXXX)
if grep -nE '[a-z]{2}-[a-z]+-[0-9]_[A-Za-z0-9]+' $(tf_files) 2>/dev/null; then
  report "literal Cognito user pool id (must come from the SSM Contract)"
fi
# sqs url carrying an account id
if grep -nE 'sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]' $(tf_files) 2>/dev/null; then
  report "SQS URL with a literal account (build it from data sources)"
fi

echo "== no terraform_remote_state across domains =="
if grep -rnE 'data\s+"terraform_remote_state"' $(tf_files) 2>/dev/null; then
  report "terraform_remote_state found (use the SSM Contract, keep state isolated)"
fi

echo "== no Lambda role with ssm:GetParameter (SSM is read at apply time) =="
if grep -rnE 'ssm:GetParameter' $(tf_files) 2>/dev/null; then
  report "ssm:GetParameter in a policy (SSM is consumed at apply time, not at runtime)"
fi

echo "== AWS provider pinned with a version constraint =="
# Every stack declaring required_providers must pin the aws provider version.
while IFS= read -r f; do
  if grep -q 'required_providers' "$f"; then
    if ! grep -A3 'aws = {' "$f" | grep -qE 'version\s*='; then
      report "required_providers without a version in $f"
    fi
  fi
done < <(tf_files)

if [ "$fail" -eq 0 ]; then
  echo "OK: no forbidden coupling found."
fi
exit "$fail"
