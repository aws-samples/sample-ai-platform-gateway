# Domain: Core (the Gateway) — Playground tab

The **central domain**. It is the man-in-the-middle gateway: it authenticates the tenant, resolves the model, applies caching, routes to the provider (with fallback and auto-cheapest), and responds in the OpenAI dialect — with **streaming (SSE)** or JSON. The other domains consume what it produces.

> "Inference" (running the model) happens at the providers; this domain is the **gateway/router**, hence "Core".

## Structure (all owned by the domain)
```
core/
├── build.sh              # compiles the Go Lambdas (arm64)
├── src/                  # Go code (hexagonal — hexagonal-refactor spec)
│   ├── go.mod
│   ├── cmd/
│   │   ├── router/       # WIRING ONLY: builds adapters → gateway.Wire → wrapper
│   │   └── keyadmin/     # API key issuance/management
│   └── internal/
│       ├── routing/      # PURE DOMAIN: decision (no SDK/network/clock; boundary_test)
│       ├── gateway/      # ORCHESTRATION shell: auth → config → guardrails →
│       │                 # limits → decision → cache → provider → telemetry
│       ├── httpapi/      # neutral INBOUND port (http.Handler, no Lambda type)
│       ├── awslambda/    # Lambda adapter (API GW event ⇄ Request/Response)
│       ├── ports/        # outbound boundaries (interfaces)
│       └── adapters/     # bedrock, openaicompat, anthropic, google, ddbconfig,
│                         # ddbcache, ddblimits, ddbkeys, ddbhints, secrets, sqsusage…
├── dist/                 # .zip artifacts (bootstrap)
└── envs/poc/             # Terraform (this domain's ISOLATED state)
```

## Local execution (outside Lambda)
The request path is a neutral `http.Handler`; Lambda is just a wrapper.
With `AIPLAT_SERVE_ADDR` set, the binary starts a local HTTP server using the
SAME code path (including API-key authentication in the handler):
```bash
cd src
AIPLAT_SERVE_ADDR=:8080 AWS_REGION=us-west-2 \
  CONFIG_TABLE=aiplat-poc-gov-config CACHE_TABLE=aiplat-poc-inf-cache \
  LIMITS_TABLE=aiplat-poc-inf-limits API_KEYS_TABLE=aiplat-poc-inf-api-keys \
  USAGE_QUEUE_URL=<sqs-url> go run ./cmd/router
# in another tab:
curl -s -X POST localhost:8080/v1/chat/completions -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" -d '{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hi"}]}'
```
It's the same path a future Fargate/App Runner deployment would use (real incremental streaming).

## AWS resources (poc)
- Lambdas (Go, `provided.al2023`, arm64): `aiplat-poc-inf-router` (**auth in the handler**: API key → tenant/app_tag) and `aiplat-poc-inf-keyadmin` (key issuance).
- DynamoDB: `aiplat-poc-inf-api-keys`, `aiplat-poc-inf-cache` (TTL).
- **Gateway:** `https://<gateway-id>.execute-api.us-west-2.amazonaws.com` (SSM contract: `/aiplat/poc/core/gateway_url`)
- **Key admin:** `https://<keyadmin-id>.execute-api.us-west-2.amazonaws.com` (header `x-admin-token`; SSM contract: `/aiplat/poc/core/keyadmin_url`)

> These URLs change every time the API Gateway is recreated (e.g., destroy/apply). The source of truth is always the SSM contract or `terraform output` — never hardcode them in other domains.

## API key issuance (onboarding)
We store only `sha256(key)`; the plaintext is returned **once**, at creation.
```bash
KA="https://<keyadmin-id>.execute-api.us-west-2.amazonaws.com/admin/keys"; T="x-admin-token: <admin-token>"
curl -s -X POST "$KA" -H "$T" -H "Content-Type: application/json" -d '{"tenant":"acme","app_tag":"web"}'  # create
curl -s "$KA" -H "$T"                                                                                      # list (no secret)
curl -s -X DELETE "$KA" -H "$T" -H "Content-Type: application/json" -d '{"id":"<api_key_hash>"}'           # revoke
```

> **Streaming:** `stream:true` returns valid SSE, but **buffered** (not token-by-token), because API Gateway does not stream. We tried a Lambda Function URL with `RESPONSE_STREAM`, but (1) a public URL is blocked by the account's public-endpoint guardrail and (2) with CloudFront+OAC/IAM, AWS requires the **client** to send `x-amz-content-sha256` on a POST with a body, which breaks the SDK drop-in.

## Supported providers (adapters)
`bedrock` · `openai_compatible` (real streaming) · `anthropic` (native) · `google`/`gemini` (native). Real streaming today is on `openai_compatible`; the others do pseudo-streaming (buffered and sliced into SSE).

## Contracts (EDA boundaries)
- **Consumes (Governance):** routing/pricing config read from the `aiplat-poc-gov-config` table. **Falls back** to environment defaults if unavailable.
- **Consumes (Models/Governance):** credentials in Secrets Manager under `aiplat/gateway/*` (resolved at runtime).
- **Produces (→ Observability):** a Usage_Record per request (`feature`, cost, `saved_usd`, `savings_reason`), **asynchronously** via SQS `aiplat-poc-obs-usage`. Never on the response path.

## Build & Deploy
```bash
./build.sh                                   # produces dist/router.zip
cd envs/poc
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform init
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform apply -var region=us-west-2
```

## Quick test
OpenAI drop-in: use `<gateway>/v1` as the `base_url` (the SDK appends `/chat/completions`).
```bash
U="https://<gateway-id>.execute-api.us-west-2.amazonaws.com/v1/chat/completions"
# JSON
curl -s -X POST "$U" -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hi"}]}'
# Streaming (SSE)
curl -sN -X POST "$U" -H "Authorization: Bearer <api-key>" -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"count from 1 to 5"}],"stream":true}'
```

> Isolation: if Governance is down, it uses default config; if a provider fails, it tries the fallback; cost capture is asynchronous and does not affect the response.
