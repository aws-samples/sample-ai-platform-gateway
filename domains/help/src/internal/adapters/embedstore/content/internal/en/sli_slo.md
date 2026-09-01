# SLI/SLO: Wilson, shrinkage and burn rate

> AIPlat internal. How we measure reliability without panicking on low volume.

## Availability SLI
`availability = good / eligible`, where **good** = served + cache and **eligible** = a success **or** a failure that counts against us (`sli_eligible=true`). **Excluded**: policy (rate/budget/guardrail/suspension), customer config and auth, and **the customer's provider quota** (`provider_quota_exceeded`). **Counted**: `provider_unreachable`/`provider_down` (dependency) and `platform`/`auth_backend_error` (us or AWS). Only `mode: sync` is included (batch has hours of latency and would destroy the error budget).

## Volume-sensitive (avoids false panic)
- **Volume floor (20):** below that → `insufficient_data`.
- **Wilson interval:** confidence around the observed rate.
- **Bayesian shrinkage (`adjusted_pct`):** weighted mean of observed × tier baseline (weight `k=50`). So 1 failure in 2 calls is not a breach, while 2% out of 1M is caught rigorously.
- **States:** `healthy` / `at_risk` (adjusted < target) / `breaching` (Wilson upper bound < target, so confident) / `insufficient_data`.
- **Target per tier:** free 99 / pro 99.5 / business 99.9.

## Burn rate (SLO) — alert-notifier
`burn = error_rate / (1 − SLO)`. Multi-window (SRE workbook): **fast burn** 14.4× over 1h → `page`; **slow burn** 6× over 6h → `ticket`. A per-window volume floor avoids noise. It alerts on the **threat to the error budget**, not on an isolated error.

## Anomaly vs the customer's own baseline
It learns the **customer's own normal** error rate (7 days, excluding the last hour) and alerts when the last hour deviates by **z-score ≥ 3** (binomial). Guards: baseline ≥ 50 eligible, current volume ≥ 10, rate ≥ max(2× baseline, 2%) and ≥ 3 errors. This catches a spike even while the global SLO still looks fine.
