## What this tab is for
Logs gives you one row per request in the period — model, provider, tokens, cost, latency, cache and result (served, cache, provider error, blocked by policy). It is the fine-grained inspection of what went through the gateway.

## How to use it
Filter by period, result and model. Blocked and failed requests cost nothing and do appear here, but they are left out of the cost summary (which counts only what was served).

## Common questions
- **Can I see the prompt or the response?** No. By policy the content is never stored — we keep only metadata and a hash used as the cache key.
- **What does "blocked" mean?** The request was stopped by a guardrail, rate limit, budget, a suspended account or a model that is not allowed.
