# Routing, fallback and auto-cheapest

> AIPlat internal. How the Core decides which provider serves each request.

## Model resolution
1. The API key resolves `org/team/app`. The effective config is read from Governance (BatchGet of `global`, `ORG#`, `ORG#…TEAM#`, `ORG#…TEAM#…APP#`, most specific wins), cached ~15s per `org|team|app`, with **fallback to defaults** when unavailable.
2. `allowed_models` for the scope filters what may be used — a model outside the list → 403; auto-cheapest never picks a model that is not allowed.
3. The attempt chain is assembled: 1st enabled = default; the rest = fallback.

## Auto-cheapest respects the list
With `auto_cheapest` on, `buildChain` **reorders by price** only the models that are **eligible and listed** (cheapest → next cheapest), instead of the manual order. Eligibility = declared capabilities (tool use, multimodal, context window). This means fallback also follows "next cheapest that can serve".

## Fallback
If the chosen model's provider fails (timeout, 5xx, unavailable), the next in the chain is tried. If a provider secret is missing, **only that model** fails and the chain continues. Fallback savings are counterfactual (baseline = the requested model).

## Provider neutrality
Adapters: `bedrock`, `openai_compatible` (OpenAI/Azure/Groq/Together/Gemini-compat/self-host), native `anthropic`, native `google/gemini`. Swapping a provider is config plus adapter — the client does not change. BYO Bedrock assumes the customer's `role_arn` at runtime (spend and quota stay in their account).
