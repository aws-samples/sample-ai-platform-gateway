# Failure taxonomy (reason / category / sli_eligible)

> AIPlat internal. How the Core classifies each request and what counts towards the SLI.

## Status in the Usage_Record
`status` ∈ `success` | `error` | `blocked` (empty = success, legacy record). The router emits a Usage_Record **on errors and blocks too** (zero cost and tokens, reusing the API Gateway `request_id`). The cost summary (`/usage/summary`) **excludes** errors and blocks so counts and latency are not skewed — those live in Logs.

## Classification fields (emitted by the Core, which is the only place that knows the cause)
- **`reason`** — short code: `rate_limit_exceeded`, `budget_exceeded`, `secret_detected`, `prompt_injection`, `model_not_allowed`, `unknown_model`, `invalid_body`, `account_suspended`, `provider_quota_exceeded`, `provider_auth`, `provider_rate_limited`, `provider_unreachable`, `provider_down`, `provider_error`, `auth_backend_error`.
- **`category`** (FCAPS) — `config` · `auth` · `policy` · `dependency` · `platform` · `capacity` · `ok`.
- **`sli_eligible`** (bool) — whether it counts towards the platform reliability SLI. **Only OUR failures count.**
- **`detail`** — short text, only for provider errors (never contains the prompt), truncated at 300 chars.

## What counts towards the SLI
- **Out** (not our failure): policy (rate/budget/guardrail/suspension), customer config and auth, and **the customer's provider quota** (`provider_quota_exceeded`).
- **In:** `provider_unreachable`/`provider_down` (dependency) and `platform`/`auth_backend_error` (us or AWS).

## SLA vs SLI nuance
`provider_quota_exceeded` (the customer's balance or quota at the provider ran out) sits **outside the SLI**, but it fires a **critical capacity alert** — the customer has nowhere left to consume from, which is not our failure but is very much their problem.
