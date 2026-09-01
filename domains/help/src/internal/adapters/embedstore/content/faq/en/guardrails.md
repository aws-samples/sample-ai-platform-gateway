## What this tab is for
Guardrails are content safety filters applied **before** the request leaves for the provider: mask PII, block secrets, stop prompt injection attempts and turn off cache storage (zero retention).

## How to use it
Turn on the guardrails you want, per scope. They apply to the next requests (allowing for the short config cache). Stopped requests appear in Logs as "blocked".

## Common questions
- **Is it infallible?** The deterministic ones (PII, secrets, no_store) are reliable. Injection detection is heuristic — it covers known cases, not everything.
- **Model-based moderation?** Not yet; today the guardrails are rule-based.
