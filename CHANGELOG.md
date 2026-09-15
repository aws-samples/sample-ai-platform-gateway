# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: the client-facing contract is the **OpenAI dialect**, so `MAJOR` is
reserved for a change that breaks a caller's request or response. Changes that need action
from an *operator* — Terraform variable defaults, deployed object layout — are `MINOR` with
an **Upgrade notes** section, which is where they are called out.

## [1.1.0] - 2026-09-14

Response streaming end to end, per-app cost attribution, and the console split out of a
single file. Driven by an external code review plus [issue #3].

### Added

- **API Gateway response streaming**, on by default (`var.response_streaming`). A response
  now reaches the client frame by frame instead of arriving whole at the end. Measured
  through the deployed Regional REST API: a 4000-token completion returns `200` over 848
  SSE frames in 56 s — the same request under a buffered integration returned
  `504 Endpoint request timed out` at 29.6 s.
- **Native provider streaming for every adapter**, through a new optional port
  (`ports.StreamProvider`): Bedrock via `ConverseStream`, Anthropic via the Messages API
  with `"stream": true`, Gemini via `:streamGenerateContent` with `alt=sse`;
  `openai_compatible` already proxied its frames verbatim. This is what makes
  time-to-first-token real: 2.9 s of a 7.4 s response, where before the first token arrived
  with the last. SSE transport mechanics are shared in `internal/adapters/ssestream` so only
  the per-dialect interpretation is written per provider.
- **`usage` and the `aiplat` block on streaming responses.** Every stream now ends
  `role → delta* → stop → final(usage + aiplat) → [DONE]`. Without it, turning streaming on
  silently gave up the per-request cost, savings and cache attribution — most of the point
  of this gateway. The frame uses `choices: []` plus extra top-level fields, the same shape
  OpenAI sends for its usage chunk, so a client reading only `choices[0].delta` is
  unaffected.
- **Per-app cost attribution on the request** ([issue #3]). A request may name an app with
  the `x-aiplat-app` header or an `app` body field, so **one key can split spend across
  projects**. Keys carry an `apps` allowlist; `keyadmin` can issue a multi-app key. The
  allowlist is deliberate: `app` selects the configuration scope, so it decides the budget,
  the rate limit, the allowed-model list and the guardrails — an unchecked value would let a
  caller charge its spend to another project's budget. An app the key does not carry is
  refused with `403 app_not_allowed`.
- **`?app=` on the usage API**, served by the per-app index (`gsi1pk = APP#<app>`) that had
  been populated since the Cost_Store was created and read by nothing. Before, one project's
  report meant reading the whole time range off a single hot partition and grouping in
  memory — work that grew with every *other* project's traffic.
- **Cache tenancy** (`cache_scope`: `team` by default, or `app` / `deployment`). Partitions
  both the response cache key and the semantic index.
- **Console:** an App filter and column in Logs, `team` and `app` in the CSV export, and a
  first-token-time badge next to total time — which is what makes it visible whether a
  provider is streaming natively or being pseudo-streamed.
- Documentation of the Go runtime choice: why `provided.al2023` is not a custom runtime, with
  the `go1.x` / `provided.al2` / `provided.al2023` support dates.

### Changed

- **`console.html` is markup only**: 6508 lines with everything inline became 1340 lines,
  plus `assets/console.css` and `assets/console.js`. Still no build step — the browser loads
  three files instead of one. The extraction was verified as a byte move by reassembling the
  original and comparing it byte for byte.
- **Terraform defaults**: `response_streaming` is now `true`, and `integration_timeout_ms` is
  `300000` (was `29000`). A streaming integration is not bound by the *Maximum integration
  timeout* quota (`L-E5AE38E3`), so the ceiling no longer depends on a quota increase. 300000
  rather than the 900000 maximum because the same value derives the router Lambda's timeout:
  a provider that accepts a connection and then stalls is billed, and holds a concurrency
  slot, until it expires.
- `cache_scope` defaults to `team`, not `deployment` — absence of configuration must not mean
  one team can be served another team's cached response.
- The semantic index sits behind a port (`ports.SemIndex`) exposing **`Search`** rather than
  get-a-list, so a backend that does server-side kNN can be substituted. Its cap moved to the
  adapter (`SEM_INDEX_CAP`, default 200).
- `ports.CostStore` gained `QueryApp`. A separate method rather than a filter argument,
  because the two are different reads: one scans a time range, the other goes straight to a
  per-app index.
- `app_not_allowed` is classified `config` and **excluded from the reliability SLI** —
  refusing correctly is not a reliability failure, and the default classification would have
  let a client looping on a typo burn the error budget.

### Fixed

- **`aws_api_gateway_deployment` hashed only resource IDs in its `triggers`.** An ID does not
  change when an attribute does, so changing an integration never produced a new deployment:
  API Gateway went on serving the old one, which answers with the right status code and an
  **empty body**. This was the actual cause of a streaming attempt that looked like a runtime
  bug, with clean CloudWatch logs throughout. Triggers now hash `uri`,
  `response_transfer_mode` and `timeout_milliseconds`.
- **Prompt-cache counters were discarded on the streaming path.** Bedrock and Anthropic report
  input tokens *excluding* cached ones, so the cache read was billed as nothing and the
  corresponding saving was never credited: cost too low **and** the saving invisible in the
  ledger.
- **A lost-write race in the semantic index.** Concurrent indexing silently dropped entries;
  now optimistic locking with a `version` attribute, a condition expression and bounded retry.
- **Budget degrade applied only to the first attempt.** The fallback chain read the config
  flag instead of the policy the decision produced, so a degrade triggered by a budget was
  abandoned on the first fallback. Its savings reason was also declared but never emitted,
  which left a console chart permanently empty.
- A Gemini chunk may carry several `parts`; only `parts[0]` was read, silently truncating
  multi-part chunks.
- Producers now stop when the consumer disconnects, instead of continuing to pull (and pay
  for) provider tokens nobody receives.

### Removed

- Dead code that described an architecture this project does not have: a Postgres schema and
  three repository stubs returning `not implemented` for Postgres/MongoDB backends. Also 49
  references to a finished refactor across 41 files, and Portuguese test and scenario names.

### Upgrade notes

1. **Your response cache is invalidated.** `cache_scope` defaults to `team`, which changes the
   key material, and existing semantic-index items under the old partition are orphaned until
   their TTL expires. Nothing is lost — the cache is regenerable — but the hit rate drops
   until it refills. Set `cache_scope: "deployment"` to keep the previous keys.
2. **If you front the site bucket with your own CDN that routes by path prefix, add a behavior
   for `/assets/*`.** The console's CSS and JS are new objects under that prefix. Without it
   the markup loads and nothing works — a page that looks almost right, which is worse than
   an obvious failure.
3. **To keep a buffered integration, set both** `response_streaming = false` **and**
   `integration_timeout_ms = 29000`. A `BUFFERED` integration is bounded by the `L-E5AE38E3`
   quota, and the provider rejects the mismatched pair at plan time.
4. **The router Lambda's timeout is derived** from `integration_timeout_ms` and is now 330 s.
   Size your provider timeouts and concurrency accordingly.
5. Existing API keys are unchanged. A key with no `apps` attribute stays single-app, and
   naming any other app is refused — which is the behaviour it had before this release.

## [1.0.0] - 2026-09-08

### Added

- Initial public release of AIPlat: an OpenAI-compatible AI gateway with built-in FinOps
  (per-request cost tracking, budgets, and an auditable savings ledger), running 100%
  serverless on AWS (Lambda, API Gateway, DynamoDB, SQS, EventBridge, Cognito, Bedrock),
  deployed per domain with Terraform.

[issue #3]: https://github.com/aws-samples/sample-ai-platform-gateway/issues/3
[1.1.0]: https://github.com/aws-samples/sample-ai-platform-gateway/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/aws-samples/sample-ai-platform-gateway/releases/tag/v1.0.0
