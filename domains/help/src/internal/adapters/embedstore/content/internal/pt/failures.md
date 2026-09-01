# Taxonomia de falhas (reason / category / sli_eligible)

> Interno AIPlat. Como o Core classifica cada requisição e o que conta no SLI.

## Status no Usage_Record
`status` ∈ `success` | `error` | `blocked` (vazio = success, registro antigo). O router emite Usage_Record **também em erro e bloqueio** (custo/tokens zero, usa o `request_id` do API Gateway). O resumo de custo (`/usage/summary`) **exclui** erro/bloqueio para não distorcer contagem/latência — eles vivem em Logs.

## Campos de classificação (emitidos pelo Core, que sabe a causa)
- **`reason`** — código curto: `rate_limit_exceeded`, `budget_exceeded`, `secret_detected`, `prompt_injection`, `model_not_allowed`, `unknown_model`, `invalid_body`, `account_suspended`, `provider_quota_exceeded`, `provider_auth`, `provider_rate_limited`, `provider_unreachable`, `provider_down`, `provider_error`, `auth_backend_error`.
- **`category`** (FCAPS) — `config` · `auth` · `policy` · `dependency` · `platform` · `capacity` · `ok`.
- **`sli_eligible`** (bool) — se conta no SLI de confiabilidade da plataforma. **Só falha NOSSA conta.**
- **`detail`** — texto curto só para erro de provedor (nunca contém prompt), truncado a 300 chars.

## O que conta no SLI
- **Fora** (não é falha nossa): política (rate/budget/guardrail/suspensão), config/auth do cliente, e **quota do provedor do cliente** (`provider_quota_exceeded`).
- **Dentro:** `provider_unreachable`/`provider_down` (dependência) e `platform`/`auth_backend_error` (nós/AWS).

## Nuance SLA × SLI
`provider_quota_exceeded` (saldo/quota do cliente no provedor esgotou) fica **fora do SLI**, mas dispara **alerta crítico de capacidade** — é o cliente sem de onde consumir, não falha nossa.
