#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# verify-decoupling.sh — guarda de CI contra regressao de acoplamento.
# Feature: multi-account-decoupling. Falha (exit 1) se achar literais proibidos
# ou anti-padroes de arquitetura nos stacks Terraform / Go versionados.
#
# Escopo: domains/**. Ignora .terraform/, dist/, tools/decoupling (testes de exemplo).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
report() { echo "FAIL: $1"; fail=1; }

# Arquivos Terraform/tftpl versionados dos dominios (exclui state e cache).
tf_files() {
  find domains -type f \( -name '*.tf' -o -name '*.tftpl' \) \
    ! -path '*/.terraform/*' 2>/dev/null
}
# Arquivos Go de runtime dos dominios (exclui o tooling de teste).
go_files() {
  find domains -type f -name '*.go' ! -path '*/.terraform/*' 2>/dev/null
}

echo "== Req 1.3: nenhum account-id de 12 digitos =="
# Exclui 200601021504: layout de tempo de referencia do Go (nao e account-id).
if grep -nE '\b[0-9]{12}\b' $(tf_files) $(go_files) 2>/dev/null | grep -vE '200601021504'; then
  report "account-id de 12 digitos encontrado (use data.aws_caller_identity)"
fi

echo "== Req 2.4: nenhuma URL/ID vivo como literal =="
# execute-api
if grep -nE 'execute-api\.[a-z0-9-]+\.amazonaws\.com' $(tf_files) 2>/dev/null; then
  report "URL execute-api literal (deve vir do Contrato SSM)"
fi
# cloudfront
if grep -nE '[a-z0-9]+\.cloudfront\.net' $(tf_files) 2>/dev/null; then
  report "dominio cloudfront literal (deve vir do Contrato SSM)"
fi
# cognito pool id (ex.: us-west-2_XXXX)
if grep -nE '[a-z]{2}-[a-z]+-[0-9]_[A-Za-z0-9]+' $(tf_files) 2>/dev/null; then
  report "id de Cognito user pool literal (deve vir do Contrato SSM)"
fi
# sqs url com conta
if grep -nE 'sqs\.[a-z0-9-]+\.amazonaws\.com/[0-9]' $(tf_files) 2>/dev/null; then
  report "URL de SQS com account literal (construir via data sources)"
fi

echo "== Req 4.4 / 8.2: nenhum terraform_remote_state entre dominios =="
if grep -rnE 'data\s+"terraform_remote_state"' $(tf_files) 2>/dev/null; then
  report "terraform_remote_state encontrado (use o Contrato SSM, mantenha state isolado)"
fi

echo "== Req 8.1: nenhuma role de Lambda com ssm:GetParameter (SSM e apply-time) =="
if grep -rnE 'ssm:GetParameter' $(tf_files) 2>/dev/null; then
  report "ssm:GetParameter em policy (consumo de SSM e em apply-time, nao runtime)"
fi

echo "== Req 8.4: provider AWS com constraint fixada =="
# Cada stack com versions/required_providers deve pinar a versao do provider aws.
while IFS= read -r f; do
  if grep -q 'required_providers' "$f"; then
    if ! grep -A3 'aws = {' "$f" | grep -qE 'version\s*='; then
      report "required_providers sem version em $f"
    fi
  fi
done < <(tf_files)

if [ "$fail" -eq 0 ]; then
  echo "OK: nenhum acoplamento proibido encontrado."
fi
exit "$fail"
