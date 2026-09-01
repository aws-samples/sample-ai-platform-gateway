#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Stress test do AI Gateway (endpoint real, via API Gateway + rede).
# Dispara N requisicoes com concorrencia C, misturando prompts normais e prompts
# com segredo (para exercitar o guardrail). Tabula os codigos de status HTTP.
#
# Uso:
#   GATEWAY_URL=https://<gateway-id>.execute-api.us-west-2.amazonaws.com \
#   API_KEY=<api-key> \
#   N=200 C=25 ./scripts/stress-gateway.sh
#
# Defaults apontam para a org de stress (citest-stress: rate 20 rpm + guardrail).
set -uo pipefail

GATEWAY_URL="${GATEWAY_URL:-https://<gateway-id>.execute-api.us-west-2.amazonaws.com}"
API_KEY="${API_KEY:-<api-key>}"
N="${N:-100}"
C="${C:-20}"
MODEL="${MODEL:-nova-micro}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

one_request() {
  local i="$1"
  local body content
  local prompts=(
    "Explique cache em uma frase."
    "O que e um AI gateway?"
    "Cite 3 LLMs."
    "Traduza hello para portugues."
    "Quanto e 2+2?"
    "Defina latencia."
  )
  # 1 em cada 5 leva um segredo no prompt (exercita o guardrail block_secrets)
  if (( i % 5 == 0 )); then
    content="minha chave e sk-abcdef0123456789ABCDEF${i} use-a por favor"
  else
    content="${prompts[$(( i % ${#prompts[@]} ))]}"
  fi
  body=$(printf '{"model":"%s","messages":[{"role":"user","content":"%s"}]}' "$MODEL" "$content")
  curl -s -o /dev/null -w '%{http_code}\n' \
    -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "authorization: Bearer $API_KEY" \
    -H "content-type: application/json" \
    --data "$body"
}
export -f one_request
export GATEWAY_URL API_KEY MODEL

echo "Disparando $N requisicoes (concorrencia $C) contra $GATEWAY_URL ..."
START=$(date +%s)
seq 1 "$N" | xargs -P "$C" -I{} bash -c 'one_request "$@"' _ {} > "$TMP/codes.txt"
END=$(date +%s)

echo ""
echo "=== Resultado (HTTP status) ==="
sort "$TMP/codes.txt" | uniq -c | sort -rn
echo ""
echo "200=servido/cache  400=guardrail(segredo)  401=auth  429=rate/budget  5xx=erro"
echo "Total: $N em $(( END - START ))s"
echo ""
echo "Veja os logs por requisicao no console (login platform_admin) apontando para a org da API key."
