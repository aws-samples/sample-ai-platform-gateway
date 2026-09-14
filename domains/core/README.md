# Domain: Core (the Gateway) — Playground tab

The **central domain**. It is the man-in-the-middle gateway: it authenticates the tenant, resolves the model, applies caching, routes to the provider (with fallback and auto-cheapest), and responds in the OpenAI dialect — with **streaming (SSE)** or JSON. The other domains consume what it produces.

> "Inference" (running the model) happens at the providers; this domain is the **gateway/router**, hence "Core".

## Structure (all owned by the domain)
```
core/
├── build.sh              # compiles the Go Lambdas (arm64)
├── src/                  # Go code (hexagonal architecture)
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

## Why `provided.al2023` (and why that is not a custom runtime)

Reviewers reasonably read `runtime = "provided.al2023"` + `handler = "bootstrap"` as a
hand-rolled runtime that someone has to keep patching. It is not, and there is no
alternative for Go:

| runtime | status |
|---|---|
| `go1.x` | **deprecated December 2023.** It no longer exists. The AWS docs state you *must* migrate to `provided.al2023` or `provided.al2` |
| `provided.al2` | **end of life 30 Jun 2026** — already past |
| `provided.al2023` | supported until **30 Jun 2029** (function creation blocked Jul 2029) |

`provided.al2023` is an **AWS-managed OS-only runtime**: AWS patches the operating
system. Nothing in this repository implements a runtime. `handler = "bootstrap"` is not
a convention we invented either — the `provided` family requires the executable to be
named `bootstrap`.

So this is the newest and longest-supported option available, not a shortcut around a
managed one. The maintenance cost is real but bounded: one OS bump before mid-2029,
which is a far cheaper event than a language runtime migration. The only way to get a
*managed* runtime would be to leave Go entirely, which would mean rewriting five
domains and giving up the arm64 cold-start and cost profile.

Do not confuse this with `github.com/aws/aws-lambda-go`, which is an ordinary Go
**library** dependency and is updated like any other (Dependabot watches it). That
library is, separately, the reason response streaming is not enabled — see below.

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

### Multi-app keys (per-request attribution)

`app` is the key's default. `apps` is the additional set the key may charge per request via
the `x-aiplat-app` header (or an `app` field in the body):

```bash
curl -s -X POST "$KA" -H "$T" -H "Content-Type: application/json" \
  -d '{"team":"platform","app":"web","apps":["api","batch"]}'
```

The allowlist is the point, not an inconvenience. `app` is the last link of the
configuration scope chain (`global → TEAM# → APP#`), so it selects the **budget, rate limit,
allowed models and guardrails**. Accepting an arbitrary app from the request would let a
caller charge its spend to another project's budget and inherit its limits — governance would
still produce a number, just the wrong one. An app the key does not carry is refused with
`403 app_not_allowed`, classified as `config` and kept **out of the reliability SLI**: the
gateway refusing correctly is not a reliability failure, and the default classification for
an unrecognised reason would have counted a client looping on a typo against the error budget.

Keys issued before this existed have no `apps` attribute and behave exactly as before:
single-app, and naming any other app is refused. `ddbkeys` reads the attribute as either a
DynamoDB string set (what `keyadmin` writes) or a list, because the two are easy to confuse
when patching an item by hand and the failure mode would be a silent 403 on every request.

> **Streaming:** `stream:true` returns SSE **frame by frame**, over API Gateway response
> streaming. On by default (`var.response_streaming`).
>
> Measured on this deployment, through the Regional REST API:
>
> | request | result |
> |---|---|
> | `stream:true`, `max_tokens:400` | 200, 11135 bytes, 61 SSE frames |
> | `stream:true`, `max_tokens:4000` | 200, 158895 bytes, 848 SSE frames, 56.1 s |
> | `stream:false` | 200, one complete JSON body |
>
> The middle row is the one that matters: the same request under a `BUFFERED` integration
> returned `504 {"message":"Endpoint request timed out"}` at 29.6 s.
>
> It takes three settings that must agree — `response_transfer_mode = "STREAM"`, the
> 2021-11-15 `response-streaming-invocations` URI, and the router's `AIPLAT_RESPONSE_MODE` —
> which is why one Terraform flag drives all three. A mismatch does not error: API Gateway
> returns the right status code with an **empty body**.
>
> Why there is a hand-written Runtime API loop (`internal/awslambda/runtimeapi.go`):
> `aws-lambda-go`'s `runtimeAPIClient.post` sets only `User-Agent`, `Content-Type` and the
> X-Ray header, never `Lambda-Runtime-Function-Response-Mode: streaming` (source read
> through v1.55.0). Returning `events.APIGatewayProxyStreamingResponse` is not enough — it
> is a format helper (metadata JSON + 8 null bytes + payload), not a transport. So the
> router owns the loop when streaming is on and calls `lambda.Start` when it is not. If that
> header ships upstream, the file is deleted and `AdaptStream` becomes the wiring.
>
> **The dead end, recorded so it is not re-diagnosed.** The first attempt returned the right
> status code with a zero-byte body and perfectly clean CloudWatch logs (START, routing
> decision, END, REPORT). Nothing in the function was wrong. The cause was
> `aws_api_gateway_deployment.router`, whose `triggers` hashed only resource *IDs* — and an
> ID does not change when an attribute does, so switching the integration to STREAM never
> created a new deployment. API Gateway went on serving the BUFFERED deployment against a
> streaming Lambda. Triggers now hash `uri`, `response_transfer_mode` and
> `timeout_milliseconds`. Generalised: when an API Gateway change appears to do nothing,
> confirm the **stage was redeployed** before suspecting the integration or the function.
>
> No Function URL is involved in any of this, which matters here: a public Function URL is
> refused by the account's public-endpoint guardrail, and fronting one with CloudFront +
> OAC/IAM would require the *client* to send `x-amz-content-sha256` on a POST with a body,
> breaking the drop-in SDK promise.
>
> One design point that survives regardless of transport: because the status line is
> committed before the first payload byte, the provider fallback chain is resolved **before**
> anything is written (`openStreamOpenAICompat` connects and validates without writing;
> `pumpOpenAICompat` then drains). That is what keeps "every provider failed → 502" honest
> instead of degrading into a 200 carrying an error frame.

> **Request time budget:** the binding limit on a completion is the API Gateway integration
> timeout, not the Lambda timeout. It is `var.integration_timeout_ms`, and the ceiling depends
> on the integration's transfer mode:
>
> | mode | ceiling |
> |---|---|
> | `STREAM` (what ships) | **900000 ms** (15 min), the Lambda service maximum, **not bound by any quota** |
> | `BUFFERED` | the `Maximum integration timeout` quota (`L-E5AE38E3`), **29000 ms** by default, adjustable for Regional and private REST APIs only |
>
> The first row is measured, not read off a doc page: STREAM at 900000 applied cleanly, and
> served a 56 s completion, on an account whose `L-E5AE38E3` is still 29000. So switching
> transfer mode buys the headroom that the quota increase would have bought, for free — the
> pending `L-E5AE38E3` request came off the critical path. The AWS provider validates the pair
> and fails the plan with
> `timeout_milliseconds must be at most N when response_transfer_mode is ...`.
>
> What ships is **300000**, not the 900000 maximum, because this value also derives the
> router's own timeout: a provider that accepts the connection and then stalls is billed, and
> occupies a concurrency slot, until it expires. 5 minutes is ~30k output tokens at the rate
> measured below — past any single chat completion — and caps that exposure at a third of what
> 15 minutes would.
>
> Independently of it, a streaming response on a Regional or private endpoint is cut after
> **5 minutes of idle time**. Token-by-token generation never triggers it; a long silent
> computation does. HTTP APIs are fixed at 30 s and support none of this.
>
> The router Lambda's own `timeout` is **derived** from that value
> (`local.router_timeout_s = min(900, ceil(ms / 1000) + 30)`), so the function always outlives the
> gateway. If it did not, the function would be killed mid-generation and the caller would get a
> 502 instead of the 504 that actually explains what happened.
>
> Sizing from measurements in this deployment: roughly **10 ms per output token** (p50 4.3 s,
> p95 16.2 s, slowest observed 39.7 s at 4096 output tokens). So the old 29 s allowed ~2.9k
> output tokens and the shipped 300 s allows ~30k. Beyond 900 s there is no synchronous option
> at all — that is an async job with polling or a webhook.
>
> The other domains' APIs set an explicit `timeout_milliseconds` slightly **above** their own
> Lambda timeout (keyadmin 17 s, config-api 17 s, usage-api 32 s, audit-api 22 s, help-api 12 s)
> for the same reason, and so a hung control-plane call does not hold the caller for the gateway
> route's much longer budget while its Lambda died early. Those are deliberately **not** raised:
> they are CRUD and query paths, and a 5-minute ceiling there would let one stuck request hold a
> concurrency slot while hiding the failure.

## Supported providers (adapters)
`bedrock` · `openai_compatible` · `anthropic` (native) · `google`/`gemini` (native).

Upstream behaviour differs and it is worth knowing which is which: `openai_compatible`
consumes a real provider stream and proxies each frame through, so the first frame is
available as soon as the provider emits it. The others fetch the complete answer and then
slice it into SSE events. Closing that gap means teaching those adapters
`InvokeModelWithResponseStream` — the IAM permission is already granted.

This is now the **remaining** limit on time-to-first-token, and it is a separate concern from
the client-facing transport above. With streaming enabled, a `bedrock` completion still shows
first-token time ≈ last-token time (measured: TTFB 55.4 s of a 56.1 s response) because the
adapter has the whole answer before the first frame is emitted. The transport is no longer
what holds it back.

## Semantic cache: the index is behind a port

`ports.SemIndex` is the boundary, and it exposes **Search**, not get-a-list-of-vectors:

```go
type SemIndex interface {
	Search(ctx context.Context, partition string, query []float32, threshold float64, semCtx, semNum string) (cacheKey string, score float64, ok bool)
	Index(ctx context.Context, partition, cacheKey string, vec []float32, semCtx, semNum string, ttlSeconds int)
	Enabled() bool
}
```

That shape is the whole point. A `Get([]SemEntry)` port would force every backend to ship
every candidate vector to the Lambda before ranking, which is precisely what a vector
engine exists to avoid — it would make a store with server-side kNN no faster than the
brute-force one.

**The shipped implementation is `adapters/ddbcache`**: one item per partition
(`SEMIDX#<partition>`), int8-quantized vectors, cosine ranked in the Lambda via the pure
domain (`internal/routing`). It costs zero extra infrastructure, which is the point of
the default. Two properties worth knowing:

- **Writes are conditional.** The index is a read-modify-write of one item, so concurrent
  invocations used to overwrite each other silently — no error, the index just grew slower
  than the traffic feeding it. There is now a `version` attribute and a bounded retry.
- **The cap is a cost dial, not a storage limit.** `SEM_INDEX_CAP` (default 200) bounds
  entries per partition. About 850 would fit in DynamoDB's 400KB item, but the whole item
  is read on every semantic lookup, so raising the cap raises the RCU per request on the
  miss path. Recall is worth paying for; paying for it silently is not.

**Adding a Valkey (or S3 Vectors, or OpenSearch) adapter** is now a self-contained
exercise: implement the three methods, wire it in `cmd/router`. There is deliberately no
stub in the tree — an adapter that cannot be deployed or tested is the same defect as the
Postgres and MongoDB backends that were just removed from `observability`.

Before choosing one, price the move honestly, because the interesting engines are not
serverless in the way the rest of this product is:

- **ElastiCache for Valkey** does vector search on **Valkey 8.2 node-based clusters** —
  not on Serverless. That means always-on nodes.
- The router has **no VPC** today (`grep -r vpc_config domains --include=*.tf` returns
  nothing). Reaching ElastiCache means creating a VPC, subnets and security groups, plus
  interface endpoints for SQS/SSM/Secrets Manager/Bedrock, a gateway endpoint for
  DynamoDB, **and a NAT gateway** — because the router calls third-party provider APIs
  over the public internet. That last one is the item people forget.
- Net effect: a deployment that currently costs nothing at idle takes on a fixed monthly
  floor, plus VPC cold starts.

The judgement embedded in the default: brute force over a few hundred vectors in-process
is the right answer for FAQ-shaped traffic, and the port exists so that a deployment with
real volume can change its mind without touching the gateway.

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
