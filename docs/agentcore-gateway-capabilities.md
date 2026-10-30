# AgentCore Gateway capabilities for AIPlat (Phase 0 discovery)

Date started 2026-10-26. Branch `feat/agentcore-gateway-full`. This document is discovery
only. It states what the Gateway offers, what AIPlat does today, and what AIPlat should
adopt, adapt, or reject. Live-measurement cells are marked UNVERIFIED until a batch in the
measurement plan has run and filled them with a date.

Sources read for this draft:

- Code at the repo HEAD on 2026-10-26: `domains/core/tf/agentcore.tf`,
  `domains/core/src/internal/adapters/bedrockgateway/bedrockgateway.go`,
  `domains/core/src/internal/gateway/gateway.go`, `domains/core/src/internal/routing/*`,
  `domains/observability/src/cmd/usage-writer/main.go`, `demo/check-agentcore.mjs`.
- AWS docs fetched 2026-10-26: gateway-targets-inference, gateway-target-inference-invokemodel,
  gateway-rules-fallback, API_CreateGatewayRule, and the 2026-10-22 what is new post.

A doc discrepancy to flag (C6): the generic `CreateGatewayRule` API reference (fetched
2026-10-26) describes conditions as principal or path matching and actions as route-to-target
or config-bundle override, while the inference fallback devguide shows `matchModels` and
`routeToTarget` with a `fallback` block. The inference-specific devguide is the one that
matches the brief. The exact request shape the deployed control plane accepts is UNVERIFIED
and is batch B7 below.

## 1. Feature matrix

Supported column legend: MEASURED (date) means AIPlat confirmed it live in `envs/poc`.
DOCS (date) means only the AWS doc asserts it. UNVERIFIED means not yet measured.

| Gateway capability | Supported | AIPlat today | Proposal | Impact on the ledger | Risk |
|---|---|---|---|---|---|
| `/inference/v1/messages` Anthropic dialect | MEASURED by 1.4.0 (code comment, undated) RE-VERIFY | Used by `bedrock_gateway` provider, Claude only | Keep | None. Current path, cache counters already honest | Low |
| InvokeModel `/inference/model/{model}/invoke` | MEASURED 2026-10-26 (router E2E, 5.4) | Used by `bedrock_invoke` provider (Nova) | Adopt (Phase 1) DONE | New transport. Cost computed from AIPlat price table, attribution verified in cost-store | Medium, pending C3 cache test |
| `/invoke-with-response-stream` | MEASURED 2026-10-26 (router E2E, 5.4) | Used by `bedrock_invoke` provider (Nova) | Adopt (Phase 1) DONE | Same as above. Streaming usage-frame is Converse event-stream, parsed by the adapter | Medium |
| `bedrock-runtime` connector target | DOCS 2026-10-26, console did not list it on 2026-10-25 | Not present. Repo has `mantle` and `runtime` provider targets | Adopt via API or IaC (Phase 1) | None directly | Medium, IaC support UNVERIFIED (B7) |
| Versioned ids with `:` on InvokeModel path | MEASURED 2026-10-26 YES | Rejected on `/v1/messages`, documented in agentcore.tf and bedrockgateway.go | ADOPT. Widen Nova and Llama routes to the gateway InvokeModel transport (R1.2) | None. Enables more routes on the gateway transport | Low (measured) |
| Dialect translation (Bedrock, OpenAI, Anthropic) | DOCS 2026-10-26 | Not used. AIPlat already owns dialects per adapter | Reject for now (see 3) | Would hide the served dialect from the adapter | Medium |
| Aggregated model list `/inference/v1/models` | RESOLVED 2026-10-26 (Phase 3): REJECT as catalog source | Console discovery uses Bedrock List APIs plus AIPlat price table | Reject — catalog is price-table-gated; an unpriced model is excluded from auto-cheapest regardless (G2/G3) | None | Low |
| Rule `matchModels` plus `routeToTarget` staticRoute with model rewrite | DOCS 2026-10-26, shape UNVERIFIED (B7) | AIPlat picks model and target in `routing.Decide` | Adapt (Phase 2), AIPlat stays the cost owner (C4) | Served model can differ from requested. Must reconcile (C2) | High |
| `weightedRoute` (A/B, canary) | RESOLVED 2026-10-26 (Phase 3): REJECT for canary | AIPlat has its own canary (`FeaturePolicy.Canary`) | Reject — AIPlat owns canary (has ledger linkage); gateway weightedRoute canary has none and would double-count | Canary already has ledger fields. Two owners would double-count | High |
| Fallback chain `targetFallback`, `triggerOn`, `firstByteTimeoutMilliseconds` | MEASURED 2026-10-26 (B4, 5.6): fallback fires, client-transparent | AIPlat has its own fallback ladder (`buildChain`) | Adapt (Phase 2), one ladder per route (C4) | One row per attempt, each a Gateway charge (C2) | High |
| Circuit breaker (ENABLED default) | PARTIAL 2026-10-26 (B5, 5.7): did NOT open across 25 deterministic 4XX invalid-model failures (behaves like UNROUTABLE, does not feed the breaker); opening needs a 5XX/connection failure, not yet measured | None | Adopt with fallback (Phase 2) | Skipped primary is not billed. Ledger must not invent a row for it | Medium |
| Per-attempt billing | MEASURED 2026-10-26 (B5, 5.7): a fallback request logs 2 outbound attempts (failed primary + fallback) | AIPlat bills model cost per served request, plus escalation attempts | Adopt accounting (Phase 2/4) | New charge line per attempt. Gateway fee per invocation UNVERIFIED (price) | High |
| Routing logs (served target and model) | MEASURED 2026-10-26 (B5, 5.7): vended APPLICATION_LOGS carry requestedModelId→resolvedModelId, targetName, reason servedViaFallbackHop:N; ~1-2 min latency; join by trace_id | None. AIPlat records served model from its own decision | Adopt (Phase 4) for reconciliation | The only source of served target under Gateway fallback (C2) | High |
| Response identifies served target or model | MEASURED 2026-10-26 (B6 via B5, 5.7): caller RESPONSE carries no indicator, but the vended routing LOG names resolvedModelId + targetName + servedViaFallbackHop | AIPlat knows the served model because it chose it | Drives reconciliation design: join the routing log by trace_id (C2) | If nothing identifies it, every Gateway-fallback row starts `unreconciled` (C2) | High |
| AgentCore Policy and Bedrock Guardrails at gateway | RESOLVED 2026-10-26 (Phase 3): guardrails stay in AIPlat; AgentCore Policy deferred | AIPlat guardrails (PII mask, secret block, injection heuristic) run before the provider call | Keep PII/secrets/injection in AIPlat (deterministic, mutate the prompt); defer AgentCore Policy (Cedar) for later cross-provider rules | None directly. Double masking avoided by one owner | Medium |
| Bedrock guardrail on a translated route | DOCS 2026-10-26: rejected with 400 | Not used | Reject on translated routes | None | Low |
| Rate limits per target, per caller, per tool | DOCS (gateway-rate-limits best practices, not yet fetched) UNVERIFIED | AIPlat enforces per-scope RPM and TPM | Keep AIPlat as enforcer (R3.2) unless a forwardable key exists | None | Medium |
| No fallback once streaming started | DOCS 2026-10-26 | AIPlat resolves the fallback chain before the first byte on stream | Align. Matches AIPlat's existing rule | A streamed attempt that fails mid-stream is one row, status error | Medium |

## 2. What AIPlat will NOT adopt (preliminary, pending measurement)

- Gateway dialect translation across providers. AIPlat already owns each dialect in its own
  adapter, and the per-dialect usage traps (Anthropic two-event usage, Gemini cumulative
  usage, cache-token inclusivity) are handled per adapter today. Letting the Gateway
  translate would hide which dialect actually served the request, which is the exact
  information the cost model needs to read token counters correctly (risk to G1 and G2).
- Bedrock Guardrails on translated routes. The AWS doc (2026-10-26) states the Gateway
  rejects a Bedrock InvokeModel request that carries a customer guardrail on a translated
  route with 400 ValidationException. Not usable there by design.
- A second fallback ladder on a route that already uses the AIPlat ladder (C4). Enabling
  Gateway fallback on such a route is rejected until the design names one owner.
- Relying on the caller-visible response to learn the served target. The AWS doc
  (2026-10-26) states the Gateway adds no caller-facing indicator of which destination
  served. Reconciliation from routing logs is the only honest source (C2).
- `/inference/v1/chat/completions` for any route with prompt cache. Already rejected in
  1.4.0 because that path returns no cache counters. Re-stated here for completeness.

## 3. Ownership decisions to confirm in design.md (checkpoint 2)

- Routing and cost: AIPlat owns model selection, auto-cheapest, budget, allowlist, scopes.
- Resilience inside one dialect: the Gateway owns fallback and the circuit breaker, on
  routes AIPlat marks for it. AIPlat's ladder skips those routes. IMPLEMENTED 2026-10-26
  (Phase 2): a config route sets `"gateway_fallback": true`, and `buildChain` then emits a
  single-step chain for it (unit-tested in characterization_test.go). The rule itself is an
  `aws_cloudformation_stack` in agentcore.tf, gated by `agentcore_rules_enabled`, OFF by
  default. One owner per route, enforced in code and provisioned in IaC.
- Canary: exactly one of AIPlat `FeaturePolicy.Canary` or Gateway `weightedRoute` per route.
  RESOLVED 2026-10-26 (Phase 3): AIPlat keeps canary ownership. `FeaturePolicy.Canary`
  already has ledger fields (`canary`, `canary_route`) that exclude experiment traffic from
  the baseline it is compared against; a Gateway `weightedRoute` canary has no such ledger
  linkage, so two owners would double-count. Gateway `weightedRoute` is therefore NOT
  adopted for canary. (A `weightedRoute` could still be used for pure load-split with no
  experiment semantics; that is out of scope here.)
- Guardrails: name the owner per check. RESOLVED 2026-10-26 (Phase 3): PII masking, secret
  blocking and the injection heuristic STAY in AIPlat (deterministic, run before the provider
  call, already measured, and they must mutate the prompt which a gateway-side check cannot do
  without seeing content twice). AgentCore Policy / Bedrock Guardrails at the gateway are NOT
  adopted now — a Bedrock guardrail on a translated route is rejected 400 by design (section
  2), and double-masking is the failure mode to avoid. AgentCore Policy (Cedar) is deferred:
  the `aws` provider has `aws_bedrockagentcore_policy`, so it is adoptable later for
  cross-provider content rules, but it buys nothing over AIPlat's deterministic checks today.
- Aggregated model list `/inference/v1/models`: RESOLVED 2026-10-26 (Phase 3): NOT adopted as
  the catalog source. AIPlat's catalog is gated by the PRICE TABLE, not by what an endpoint
  lists — a model without a price is excluded from auto-cheapest (G2/G3) regardless of whether
  the gateway advertises it. Merging the gateway list would add names AIPlat cannot price or
  rank, so it adds no FinOps value. Discovery stays price-table-driven.
- Rate limits: AIPlat stays the per-scope enforcer unless B-rate proves a forwardable key.
  RESOLVED 2026-10-26 (Phase 3): AIPlat remains the enforcer (per-scope RPM/TPM in
  `limits.go`); gateway rate limits are not adopted because AIPlat's limits are per
  team/app/org scope and the gateway key is per target, which cannot express the same scopes.

## 4. Measurement plan (costed, per C8)

All batches run in `envs/poc`, region us-west-2 for the deployment and us-east-1 for the
gateway (the gateway region today, see agentcore.tf). Models: Claude Haiku 4.5 and Amazon
Nova Micro. Token cost per test call is a few hundred tokens, which is a fraction of one
cent. The dominant unknown is the Gateway per-invocation fee, marked UNVERIFIED (price), to
confirm from the Bedrock AgentCore pricing page before B1. No batch runs until the human
approves this plan (checkpoint 1).

| Batch | Goal | Creates | Calls | Models | Est. model spend |
|---|---|---|---|---|---|
| B1 | InvokeModel path works, and whether `:` ids work on it | 1 bedrock-runtime connector target | ~8 | Nova Micro, 1 Llama, 1 Cohere embed, 1 Claude `-v1:0` | < $0.05 plus 8 gateway invocations |
| B2 | Streaming on `/invoke-with-response-stream`, first vs last token, usage-frame shape | reuses B1 target | ~4 | Haiku 4.5 | < $0.05 plus 4 invocations |
| B3 | Cache counters byte-identical to direct Bedrock, per path (C3) | none new | ~6 | Haiku 4.5 | < $0.10 plus 6 invocations, prompt cache write adds a premium |
| B4 | Fallback triggers (503, 429, first-byte timeout) and circuit breaker open and close timing, and per-attempt billing | 1 stub primary Lambda (503/sleep/OK by env var), 1 provider target for it, 1 gateway rule with fallback | ~15 | Haiku 4.5 as the healthy fallback | < $0.10 plus up to ~25 invocations (failed primaries count) |
| B5 | Routing logs: fields, latency until available, join key to the AIPlat request id | enable routing logs sink | ~6 | Haiku 4.5 | < $0.05 plus 6 invocations |
| B6 | Response headers and body: does anything identify the served target or model | folded into B1 and B4 | 0 extra | n/a | $0 |
| B7 | Terraform support: which `aws` provider version covers the bedrock-runtime connector and gateway rules, or whether to use awscc, CloudFormation, or a provisioning Lambda | none (read provider schema and docs) | 0 | n/a | $0 |

Total estimated model spend across B1 to B5 is under $0.40. The gateway invocation fee is
UNVERIFIED (price) and must be confirmed first. Fallback batches intentionally generate
failed primary attempts, which the AWS doc (2026-10-26) says are billed as gateway
invocations, so B4 is the batch to watch for cost.

## 5. Results

### 5.1 B7 Terraform support. MEASURED 2026-10-26 (provider schema of `aws` 6.71.0)

Inspected the real `aws` 6.71.0 resource schemas. The result is split, not uniform.

TERRAFORM-SUPPORTED (Phase 1 and Phase 3 provisioning stays in `agentcore.tf`):

- `aws_bedrockagentcore_gateway_target` has `target_configuration { inference { provider {
  endpoint, operation { path, provider_path, model { model } } } } }` and also a
  `connector { source { connector_id } }` form. The InvokeModel target is a PROVIDER config
  with operations `/model/{model}/invoke` and `/model/{model}/invoke-with-response-stream`,
  NOT the connector. Reason: the repo already uses provider targets on purpose. A connector
  runs model discovery at creation time as an AWS-owned principal that does not satisfy the
  confused-deputy `aws:SourceAccount`/`aws:SourceArn` conditions on the execution role (see
  the comment on the `mantle` target in `agentcore.tf`). Keeping a provider config preserves
  those conditions and needs no new IAM (the role already grants `bedrock:InvokeModel` and
  `bedrock:InvokeModelWithResponseStream` on `foundation-model/*` and `inference-profile/*`).
- `aws_bedrockagentcore_policy` (with a `definition { cedar { statement } }`) and
  `aws_bedrockagentcore_policy_engine` are present, so AgentCore Policy for Phase 3 is
  Terraform.

NOT TERRAFORM-SUPPORTED YET (Phase 2 fallback needs another path):

- `aws_bedrockagentcore_gateway_rule` 6.71.0 is the GENERIC rule, not the inference rule.
  Its `condition` has only `match_paths` and `match_principals`. Its `action` has
  `route_to_target { static_route { target_name } | weighted_route { traffic_split } }` and
  `configuration_bundle`. There is NO `match_models`, NO `fallback` / `targetFallback` /
  `triggerOn` / `firstByteTimeoutMilliseconds` / `circuitBreaker`, and `static_route` has no
  model rewrite. So the 2026-10-22 inference fallback feature is not yet in the provider.
  Phase 2 options, to decide in Phase 2 with a short spike: the `awscc` provider (check
  whether it carries the inference rule shape), a CloudFormation resource, or a small
  provisioning Lambda that calls `CreateGatewayRule`. The local AWS CLI cannot be used for a
  one-off here (it predates AgentCore, no `bedrock-agentcore-control` command).

Environment note (2026-10-26): the AWS CLI on this workstation predates AgentCore, so
ad-hoc control-plane calls via the CLI do not work here. Provisioning of targets and policy
is Terraform. Data-plane measurement calls go through the AIPlat router, which already signs
SigV4 for service `bedrock-agentcore`.

### 5.2 Pricing. PARTIAL 2026-10-26

The public AgentCore pricing page (fetched 2026-10-26) lists Gateway tool pricing
(InvokeTool at $5 per million, Search at $25 per million, Web Search at $7 per 1,000). It
does not list a separate per-invocation price for inference targets. The inference-target
invocation fee is therefore UNVERIFIED (price). Conservative assumption for the cost guard:
even at the tool rate of $5 per million, a few hundred measurement calls cost a fraction of
one cent in gateway fees, so model-token spend (under $0.40 total across B1 to B5) is the
binding cost. Confirm the real inference invocation price before Phase 2 production use.

### 5.3 Live scenarios

B1 ran 2026-10-26 against the deployed `invoke` target (us-east-1 gateway). Findings:

- The `:` in a model id is NOT rejected on the InvokeModel path. `us.amazon.nova-micro-v1:0`
  returned HTTP 200 with a valid response. The restriction documented for `/v1/messages`
  does not apply when the model id travels in the URL path.
- Both SigV4 signing services work: `bedrock-agentcore` and `bedrock` each returned 200. The
  adapter will keep `bedrock-agentcore` for consistency with the Messages transport.
- The response is the NATIVE Bedrock schema for the family, not the Anthropic Messages
  shape. Nova returned `{"output":{"message":{"content":[{"text":...}]}},"stopReason":...,
  "usage":{"inputTokens","outputTokens","totalTokens","cacheReadInputTokenCount",
  "cacheWriteInputTokenCount"}}`. So the InvokeModel transport needs its own response parser
  (per-family native), it cannot reuse the Anthropic adapter. Cache counter FIELDS are
  present on this path (value 0 on an uncached call). Byte-identity vs a direct Bedrock
  InvokeModel call is B3.
- A FAILED-then-fixed operational finding: a bedrock-runtime InvokeModel target runs a model
  catalog scan at creation AS the execution role, so it needs `bedrock:ListFoundationModels`
  (and the sibling list actions). Added in `aws_iam_role_policy.agentcore_invoke_discovery`.
  IAM is eventually consistent, so the first apply after adding the grant can still 403. A
  `time_sleep` between the grant and the target is the one-shot fix (tracked for the test
  pass).

| Scenario | Path | Model | Attempts | Latency | Gateway charge | Model cost | Cache counters | Ledger row status |
|---|---|---|---|---|---|---|---|---|
| B1 id with colon | /inference/model/{id}/invoke | us.amazon.nova-micro-v1:0 | 1 | fast (~1s) | 1 invocation (price UNVERIFIED) | ~$0.0000004 | fields present, 0 on uncached | n/a (direct probe) |
| B2 OpenAI-dialect | /inference/v1/chat/completions | us.amazon.nova-micro-v1:0 | 1 | n/a | 0 | 0 | n/a | HTTP 400, Nova rejects `max_tokens` on the translated path |
| B2 native stream | /inference/model/{id}/invoke-with-response-stream | us.amazon.nova-micro-v1:0 | 1 | ~1s TTFB | 1 invocation | ~$0.0000004 | Converse-style event-stream (`application/vnd.amazon.eventstream`) | n/a (direct probe) |
| E2E non-stream (via router) | router `/v1/chat/completions` → `bedrock_invoke` → /invoke | us.amazon.nova-micro-v1:0 | 1 | 1217 ms | 1 invocation | $0.000001155 (9 in, 6 out) | reported, 0 (uncached) | success, recorded in cost-store |
| E2E stream (via router) | router `/v1/chat/completions` stream:true → /invoke-with-response-stream | us.amazon.nova-micro-v1:0 | 1 | 759 ms | 1 invocation | $0.00000868 (8 in, 60 out) | reported | success, recorded in cost-store |
| (pending B3 cached, B4, B5) | | | | | | | | |

### 5.4 Router end-to-end. MEASURED 2026-10-26

The full path — deployed poc router → `bedrock_invoke` adapter → AgentCore Gateway `invoke`
target → Amazon Nova — was exercised live against `https://1yhzvlxl3m.execute-api.us-west-2
.amazonaws.com/prod/v1/chat/completions` with a scoped API key, after adding a
`gw-nova-micro` route (provider `bedrock_invoke`, `provider_model_id us.amazon.nova-micro-v1:0`,
base_url the gateway, region us-east-1) to the poc config. Findings:

- Non-streaming returns HTTP 200 with the OpenAI-shaped body, `provider: bedrock_invoke`,
  `served_model_id: us.amazon.nova-micro-v1:0`, and `usage` (prompt 9 / completion 6).
- Streaming (`stream: true`) returns the SSE frames token by token, a terminal
  `finish_reason: stop`, a trailing `aiplat` usage frame (prompt 8 / completion 60), then
  `[DONE]`. This is the new `bedrockinvoke` `StreamProvider` path
  (`OpenStream` → `/invoke-with-response-stream` → event-stream decode → SSE).
- Ledger parity (honesty check): both calls were written to `aiplat-poc-obs-cost-store` with
  `status: success`, `provider: bedrock_invoke`, `served_model_id us.amazon.nova-micro-v1:0`,
  token counts identical to the response, `price_source: list`, and the gateway host recorded
  as `upstream`. The cache counters were `reported` (0 on an uncached call), preserving the
  reported-vs-absent distinction. A deliberately-induced provider failure (the router was hit
  once before its code was redeployed, so the provider case was missing) was recorded as
  `status: error`, `reason: provider_error`, `sli_eligible: true`, zero cost — no phantom
  spend row.
- Total measured model spend for the two E2E calls: ~$0.0000098, plus 2 gateway invocations
  (fee UNVERIFIED, see 5.2). The failed attempt made 0 gateway calls (it failed in
  `providerFor` before signing).

Test fixtures used (poc, to clean up after the full test pass): the additive `gw-nova-micro`
entry in `ORG#XYZ_ORG` config (routing + pricing + model_order + allowed_models) and one
API-key record in `aiplat-poc-inf-api-keys` (note field marks it as the e2e test key).

B2 findings (2026-10-26):

- The gateway OpenAI-dialect path `/inference/v1/chat/completions` is NOT a clean reuse for
  Nova. A standard OpenAI body with `max_tokens` returns HTTP 400 "extraneous key
  [max_tokens] is not permitted" (the gateway validates against the Nova schema after
  relocating the id). So the `openaicompat` adapter cannot be reused as-is for Nova on the
  gateway. Decision: the InvokeModel transport uses the NATIVE path, not the OpenAI-dialect
  path.
- The native `/model/{id}/invoke-with-response-stream` returns the AWS binary event-stream
  (`application/vnd.amazon.eventstream`) with Converse-shaped chunks (`messageStart`,
  `contentBlockDelta.delta.text`, `contentBlockStop`, and a trailing metadata chunk with
  usage). This is the shape the existing `bedrock` ConverseStream parser already understands,
  so the new adapter reuses that parsing rather than inventing one.
- Design decision for the InvokeModel adapter: native path, per model family. Start with
  Amazon Nova (native InvokeModel body is the messages/inferenceConfig shape, response is
  `output.message.content[].text` plus `usage.{inputTokens,outputTokens,
  cacheReadInputTokenCount,cacheWriteInputTokenCount}`). Meta Llama has a divergent native
  schema (`prompt`/`generation`) and is added only if a route needs it.

### 5.5 Phase 2 provisioning spike (gateway rules / fallback). MEASURED 2026-10-26, no cost

Resolves the Phase 2 open question (which of `awscc` / CloudFormation / a provisioning Lambda
carries the inference rule shape). No resources were created and no calls were billed — this
is a schema inspection only.

- `aws` provider 6.71.0 `aws_bedrockagentcore_gateway_rule`: GENERIC only (match_paths /
  match_principals → route_to_target / configuration_bundle). No inference fields. (5.1)
- `awscc` provider `awscc_bedrockagentcore_gateway_rule` (pulled latest): ALSO generic —
  has `weighted_route` / `route_to_target` / `static_route` but NO `match_models`,
  `fallback`, `circuit_breaker`, `trigger_on`, or `first_byte_timeout`. awscc does not expose
  the inference rule either.
- CloudFormation registry type `AWS::BedrockAgentCore::GatewayRule` (us-east-1): HAS the full
  inference feature. Confirmed definitions and field shapes:
  - `Conditions`: `MatchModels { AnyOf: [{ Value, Operator: StringEquals|StringLike }] }`
    (also MatchPaths / MatchPrincipals).
  - `Actions`: `RouteToTarget { StaticRoute { TargetName, Model?, Fallback } | WeightedRoute }`
    (also ConfigurationBundle).
  - `Fallback { TargetFallback: [{ TargetName, Model?, TriggerOn, FirstByteTimeoutMilliseconds }]
    (1-2 entries), TriggerOn: { StatusCodes: ["4XX".."5XX"/429...], Errors:
    [CONNECTION_ERROR|FIRST_BYTE_TIMEOUT|UNROUTABLE] }, FirstByteTimeoutMilliseconds (50-900000),
    CircuitBreaker { Mode: ENABLED|DISABLED (default ENABLED) } }`.

DECISION: provision Phase 2 rules via CloudFormation, driven from Terraform with an
`aws_cloudformation_stack` resource (gated by its own off-by-default flag, like the invoke
target). This keeps provisioning in the same IaC, needs no provisioning Lambda, and no
`awscc` dependency. The data-plane BEHAVIOR (actual fallback triggers, circuit-breaker
open/close timing, per-attempt billing) is still the costed batch B4, which intentionally
generates billed failed-primary attempts and is therefore NOT run until the human approves
the spend.

### 5.6 B4 model fallback + circuit breaker. MEASURED 2026-10-26

Provisioned a real inference rule via CloudFormation (`AWS::BedrockAgentCore::GatewayRule`,
stack `aiplat-b4-fallback-rule`, us-east-1) against the live gateway and exercised it with
direct SigV4 calls to `/inference/model/{model}/invoke` (no router involved), then deleted
the stack. Rule: `MatchModels [fallback-probe]` → `StaticRoute { TargetName: invoke, Model:
us.amazon.nova-micro-BADID-v1:0 (deliberately invalid), Fallback { TriggerOn StatusCodes
[4XX,5XX,429] + Errors [...], FirstByteTimeoutMilliseconds 5000, TargetFallback [{ invoke,
us.amazon.nova-micro-v1:0 }], CircuitBreaker ENABLED } }`.

Findings:

- PROVISIONING CONFIRMED. The CloudFormation type created the inference rule cleanly
  (CREATE_COMPLETE), attached by `GatewayIdentifier`. This validates the Phase 2 path end to
  end: `aws_cloudformation_stack` from Terraform is viable, no provisioning Lambda, no awscc.
- MODEL FALLBACK CONFIRMED (client-transparent). Baseline: `fallback-probe` with no rule →
  HTTP 404 "not found on any target". A bad model id on the target → HTTP 400 "model
  identifier is invalid". WITH the rule, `fallback-probe` → HTTP 200 with a valid Nova
  answer. The only path to 200 is: rule matched → primary (invalid id) returned 400 →
  TriggerOn 4XX fired → fell back to the healthy Nova destination. The caller sees one clean
  response and no indicator of which destination served it, exactly as documented.
- CIRCUIT BREAKER: DIRECTIONAL, not conclusive from the client. Across a 10-call burst,
  latency drifted from ~1.15s on the first calls to ~1.0s and held — consistent with the
  breaker opening after consecutive primary failures and then SKIPPING the ~0.15s failed-
  primary attempt. Conclusive confirmation (and the per-skip accounting) needs the vended
  routing logs, which note when the breaker skipped a destination — that is batch B5 and the
  logs were not enabled here.
- PER-ATTEMPT BILLING: AWS-DOCUMENTED, not independently measured. The devguide states each
  outbound attempt (including the failed primary before fallback) is one gateway invocation,
  and the breaker reduces this by skipping a known-failing primary. No CloudWatch metric
  namespace for the gateway was populated in this account (checked AWS/BedrockAgentCore and
  variants — empty), so the invocation count could not be read here; it is a routing-log /
  billing-report confirmation (B5) rather than a client-side one.

Cost of B4: ~13 gateway calls (3 baseline + 10 burst), each successful fallback request up
to 2 invocations, plus a few hundred Nova tokens — a fraction of one cent in model spend.
The inference per-invocation gateway fee remains UNVERIFIED (price), see 5.2.

The rule stack and the probe were deleted afterward; the gateway is back to default routing
(`fallback-probe` → 404 re-confirmed).

### 5.7 B5 routing logs + B6 served-target attribution. MEASURED 2026-10-26

Enabled the gateway's vended APPLICATION_LOGS via the standard CloudWatch delivery chain
(`logs put-delivery-source` with the gateway ARN + log-type APPLICATION_LOGS →
`put-delivery-destination` → `create-delivery`), log group
`/aws/vendedlogs/bedrock-agentcore/gateway/APPLICATION_LOGS/<gateway_id>`. Then provisioned a
probe fallback rule (priority 50, MatchModels `fallback-probe`, StaticRoute invoke/BADID with
a healthy-Nova TargetFallback) and sent a 25-request burst. Logs appeared ~1-2 min later.

Findings (all CONFIRMED from the logs):

- PER-ATTEMPT BILLING CONFIRMED. Each request_id emits TWO
  `Executing Http request for target <id> - RequestId: <rid>` lines — the failed primary
  (invalid model id, HTTP 400) and the fallback — i.e. two outbound attempts, each billed as
  one gateway invocation. This is the direct measurement the B4 note was missing.
- SERVED-TARGET ATTRIBUTION CONFIRMED (B6 resolved). Each request emits a routing record:
  `{"outcome":"QUALIFIED","matchType":"QUALIFIED_OVERRIDE","requestedModelId":"fallback-probe",
  "resolvedModelId":"us.amazon.nova-micro-v1:0","targetName":"invoke",
  "requestPath":"/model/fallback-probe/invoke","reason":"servedViaFallbackHop:1"}`. So while
  the CALLER response carries no served-destination indicator, the vended log DOES — this is
  the reconciliation source (C2). `reason: servedViaFallbackHop:N` names which hop served.
- RULE MATCH LOGGED: `Action resolved from rule <ruleId> (priority 50.0): Route to target:
  targetName=invoke`. The rule id and priority are in the log.
- JOIN KEY / LATENCY. The gateway log carries `request_id`, `trace_id`, `span_id`. The join
  to AIPlat's own request id is by `trace_id`/`span_id` or timestamp correlation (the gateway
  does not see AIPlat's request id); when AIPlat calls the gateway it can propagate its id as
  a header for a direct join (future work). Logs were queryable ~1-2 min after the traffic.
- CIRCUIT BREAKER — did NOT open for this failure type. Across all 25 consecutive identical
  failures every request still showed 2 attempts (primary not skipped). Consistent with the
  primary failure being a deterministic 4XX "invalid model identifier", which behaves like
  the documented UNROUTABLE case ("advances the chain but does not feed the circuit breaker").
  Forcing the breaker open therefore needs a 5XX / connection-type failure (a stub primary
  that returns 503), not run here to keep cost and scope bounded. Net: the breaker is real but
  a 4XX invalid-model fallback pays the failed-primary invocation every time.

B5 cost: 25 gateway calls × 2 attempts ≈ 50 invocations + a few hundred Nova tokens — a
fraction of a cent. The probe rule, the log delivery, and the log group were all deleted
afterward; the shipped Phase 2 module rule (nova-micro → nova-lite) remains.

### 5.8 Phase 4 reconciliation: correlation-id join is infeasible. MEASURED 2026-10-26

Before building a per-row reconciler, tested whether AIPlat's request id can be joined to a
gateway log line. The router now forwards `X-Aiplat-Request-Id` to the gateway (before SigV4
signing). Sent a tagged call and inspected the vended routing log: the correlation id does
NOT appear anywhere in the log. The gateway logs only its OWN `request_id` / `trace_id` /
`span_id`, plus the routing record (`requestedModelId`, `resolvedModelId`, `targetName`,
`reason: servedViaFallbackHop:N`, `routingRuleId`). A caller-supplied header is not echoed.

Consequence for the design: there is NO key to join a gateway routing-log line back to a
specific AIPlat ledger row. So Phase 4 does two honest things instead of one impossible one:

1. CORE side (shipped): a `gateway_fallback` route's Usage_Record carries
   `gateway_owned=true` + `reconcile_state="pending"`. AIPlat admits it does not know the
   true served model or attempt count for these rows (the gateway response is transparent).
   No false precision. (`decorateGatewayOwnership`, unit-tested.)
2. RECONCILER (shipped, off by default — `agentcore_reconciler_enabled`): the gateway's
   vended routing logs are delivered to CloudWatch and a Lambda (`cmd/usage-reconciler`,
   pure parsing in `internal/reconcile`) records the gateway-side TRUTH as one idempotent
   GATEWAY fact row per gateway request in the Cost_Store (`pk="GATEWAY"`,
   `sk="REQ#<gatewayRequestId>"`): resolved model, target, served fallback hop, and the
   invocation count (`hop+1` = per-attempt billing). The ROI/reporting reads this parallel
   table for the served-target mix and invocation counts; it is the honest form of
   "routing-log reconciliation" given the missing join key. The `X-Aiplat-Request-Id`
   header is still sent so that, if AWS later echoes caller ids into the log, a true per-row
   join becomes a one-function change with no schema churn.

The still-missing dollar figure is the gateway per-invocation PRICE (UNVERIFIED, 5.2): the
reconciler counts invocations, but the fee to multiply them by is not published.

## 6. Open questions

Resolved 2026-10-26:

- Terraform support (was B7). RESOLVED. `aws` provider 6.71.0 has
  `aws_bedrockagentcore_gateway_rule`, `connector` and `operations` on the target, and
  `aws_bedrockagentcore_policy`. All provisioning stays in Terraform. See 5.1.
- Rule request shape AND provisioning path. RESOLVED (5.5). The inference rule
  (`MatchModels` + `RouteToTarget` + `Fallback` + `CircuitBreaker`) is NOT in the `aws` or
  `awscc` providers; it IS in the CloudFormation type `AWS::BedrockAgentCore::GatewayRule`.
  Phase 2 provisions it via `aws_cloudformation_stack` from Terraform. The data-plane
  behavior is still measured in B4 (costed).

Still open (measured in the batches):

- Inference-target per-invocation price (UNVERIFIED price). See 5.2.
- `:` in model ids on the InvokeModel path (B1).
- Cache counters byte-identical on `/invoke` and `/invoke-with-response-stream` (B3).
- Streaming usage-frame shape on `/invoke-with-response-stream` (B2).
- Fallback triggers, circuit breaker timing, per-attempt billing (B4).
- Routing-log fields, latency until available, join key to the AIPlat request id (B5).
- Whether any response field identifies the served target or model (B6).
