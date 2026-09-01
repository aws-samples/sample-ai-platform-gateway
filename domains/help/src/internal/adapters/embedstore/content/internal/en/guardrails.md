# Deterministic guardrails

> AIPlat internal. Content filters applied in the Core before the request leaves.

Read from `config.guardrails` (effective scope) and applied **before** the request goes to the provider:

- **`mask_pii`** — masks e-mail, tax ID, card and phone in the prompt (regex) before sending.
- **`block_secrets`** — if the prompt contains a key, token or secret, refuse with `400` (`policy_violation`, code `secret_detected`).
- **`block_injection`** — **heuristic** prompt-injection detection (classic patterns) → `400` `prompt_injection`. It is not a model; it covers known cases and is not infallible.
- **`no_store`** — turns off the response cache for that org (zero retention): it never reads nor writes content to the cache.

The deterministic ones (mask/secret/no_store) are reliable; **model-based moderation does not exist yet**. A guardrail is **data** (config), not a deploy — it changes for the next requests, allowing for the short config cache (~15s).

Blocked requests enter the Usage_Record as `blocked`, `category = policy`, zero cost and tokens, and show up in the Logs tab (not in the cost summary).
