# Rate limit and budget

> AIPlat internal. How the invoice and noisy-neighbour guardrails are applied.

## Where the counters live
In the Core's own table (`*-limits`, with TTL) — there is **no** synchronous read of Observability's Cost_Store (that preserves isolation between domains). The Cost_Store is the historical, authoritative source; the counters are an operational guardrail (an approximation).

## Scope of the limit
The counter uses the scope that **defined** the policy. A limit on the org counts for the whole org; on a team, only for that team (inheritance `TEAM# → APP#`).

## Rate limit
Fixed one-minute window. `requests_per_minute` uses an atomic `ADD` in DynamoDB (blocks on exceed); `tokens_per_minute` is accumulated after the response and stops the *next* request. The response is `429` in OpenAI error format (`rate_limit_exceeded`) with `retry-after`.

## Monthly budget
Accumulated spend per scope and month. Actions:
- **`alert`** — keeps serving and flags `budget_state`.
- **`degrade`** — forces the cheapest allowed model (produces counterfactual saving `budget_degrade`).
- **`block`** — `429` `insufficient_quota`.

The `aiplat.budget_state` field in the response exposes the state (`""`, `exceeded_alert`, `exceeded_degraded`).

## Propagation
A policy change takes effect within ~15s (config cache per Lambda instance).
