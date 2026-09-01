# Exact and semantic cache

> AIPlat internal. How the gateway reuses answers and why the key includes the org.

## Exact cache
- **Key:** `sha256(org | model | messages)` — the org is part of the key, so a cross-tenant hit is **impossible** (structural isolation, verified by test).
- **TTL:** `config.cache_ttl` (0 = off). An exact hit is **verified** saving (same model, avoided cost is observable), `savings_reason = cache`.
- `no_store` (guardrail) turns the cache off for that org (zero retention): it neither reads nor writes.

## Semantic cache (GPTCache-style, our own take)
- Opt-in (`semantic_cache`), independent of the exact cache — you can enable only this one.
- After an exact/canonical miss, if enabled: **embed** the question (Titan v2, dim 256) → search the index by **cosine** → serve when similarity ≥ `semantic_threshold` (0.92 balanced; 0.95 precise; 0.88 aggressive).
- **Per-tenant index:** a single `SEMIDX#<org>` item in the cache table (no new infrastructure). Vectors are int8-quantised to fit.
- Savings are recorded as `semantic_cache` (**counterfactual/approximate** — it admits false positives). Cost and latency: ~60 ms of vectorisation per miss; each hit avoids 100% of that call's cost.

## Provider prompt caching (prefix cache)
Per route (`prompt_cache`), only where the provider supports it (Bedrock/Anthropic): the gateway marks the end of the system message to cache the stable prefix; later reads with the same prefix cost ~90% less (the first write charges a premium). Recorded as verified saving `provider_prompt_cache`.
