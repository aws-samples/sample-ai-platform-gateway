# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: the client-facing contract is the **OpenAI dialect**, so `MAJOR` is
reserved for a change that breaks a caller's request or response. Changes that need action
from an *operator* — Terraform variable defaults, deployed object layout — are `MINOR` with
an **Upgrade notes** section, which is where they are called out.

## [Unreleased]

## [1.2.0] - 2026-09-16

Model access became a real hierarchy: the level above is now a **ceiling** that a team or
app can narrow but never widen, and there is a screen that shows every scope at once
instead of one at a time. Plus the Bedrock catalog stopped being a hardcoded list.

### Upgrade notes

Read these two before deploying — both change behaviour on data you already have.

- **`allowed_models` now resolves by INTERSECTION down the scope chain, not by
  replacement.** Before, a list at a more specific scope replaced its parent's, so an app
  allowing `[a,b,c]` under a team allowing `[a]` really was served `b` and `c`. Now the
  ceiling wins and that app is served `[a]` only. **Any scope currently configured to widen
  its parent will lose the extra models.** The console lists exactly those scopes in the
  warning strip above the matrix, so the set is finite and reviewable before you deploy;
  measured on the reference deployment it was empty. The rule is applied in the Core
  (`ddbconfig`) and mirrored in Governance, with both sides verified against the shared
  fixture `testdata/contracts/config-scope/scope-chain.json`.
- **A DECLARED empty `allowed_models` now denies everything. It used to allow
  everything.** `Config.allowed` tested `len(list) == 0`, so writing an empty list — which
  is what turning every model off in the old console did — granted the entire catalog: the
  exact opposite of the intent. An *absent* key still means "no restriction"; only a
  present-but-empty list is a denial. This distinction is required by the intersection
  rule, which legitimately produces the empty set when two scopes declare disjoint lists,
  and reading that as "unrestricted" would turn two restrictions into full access. Scan for
  affected scopes before deploying: any config item whose `allowed_models` is `[]` flips
  from allow-all to deny-all. On the reference deployment there were none.

### Added

- **A model access matrix: every scope in one request.** `GET /admin/access/matrix` returns
  the served model set for the org, every team and every app, together with each scope's own
  declaration, its ceiling, and the models it declares that the ceiling refuses. The console
  renders it as a grid in **Limits & Budget**, replacing the per-scope pill row that could
  only answer "what does THIS scope get?" — answering "where do our teams and apps differ?"
  used to cost 2×N round trips and had no screen at all. Reads run in three parallel waves
  (org → teams → apps), reusing each parent's merge, so it costs `2 + teams + apps`
  GetItems; past 400 scopes it answers `truncated: true` rather than timing out.
- **The parent's ceiling is visible and unclickable.** A model the level above denies cannot
  be switched on in a child scope, so the escalation path is absent rather than discouraged.
  The states are distinct on shape as well as colour: allowed here, inherited, denied here,
  forbidden above, and *declared here but denied above* — that last one is a leftover
  declaration which is **not** served and can be removed in one click.
- **Guard against writing an empty allowlist.** Saving a scope with every model off is
  refused, with the reason on screen: the gateway reads an empty list as no restriction, so
  it would grant everything.

### Fixed

- **The matrix read the org's own config from the wrong key.** The org's ancestry was taken
  from `govcore.ScopeKeys(org,"","")`, which appends `ORG#<org>#TEAM#default` — a *sibling*
  of the other teams, not an ancestor of the org. Reading that tail as "the org's own
  config" made the org's `allowed_models` invisible (`has_own: false` with the list plainly
  stored) and made `orgEff` fall back to the whole catalog, so **every team was shown a
  ceiling more permissive than the org's real one** — the one guarantee the screen exists to
  provide. Covered by a test that fails against the previous code.
- **`?effective=1` reset the ceiling halfway down the chain.** The accumulated ceiling was
  read back out of the merged map each round, where the resolved value is a `[]string` while
  a freshly decoded document holds `[]interface{}`; the type assertion failed silently and
  the ceiling collapsed to the last level that declared one. Found by probing the live API —
  the contract test drives the pure rule and never crosses a map, so it could not see this.
- **A stale alias no longer produces a warning with nothing to click.** An alias removed
  from Models & Routing but still named in a scope's list has no column in the matrix, since
  columns come from the catalog. Lists are now clipped to the catalog, which is accurate: a
  model with no route cannot be served whatever any list says.
- **`allowed_models` had two owners in the console and the wrong one could win.** The
  Limits & Budget form wrote it alongside rate limit and budget, so an unrelated budget save
  could revert a model-access decision — the same class of bug as the `cache_scope` drop in
  1.1.1. The matrix is now its only owner; the limits form round-trips the field untouched.
- **The app selector offered every app in the org under any team**, so a save could land on
  `ORG#o#TEAM#a#APP#b` — a scope key no API key ever resolves. It reported success and did
  nothing, permanently. The selector is now filtered to the selected team's apps.
- **Bedrock model discovery listed ~12 of the ~103 models you can actually invoke.**
  Two independent defects added up to that number. The console's Bedrock dropdown was a
  hardcoded table in `console.js`, so it only ever showed what someone last typed into it.
  And the live "fetch models from my account" path called `ListFoundationModels` alone,
  which returns *bare* model ids — but most of the current generation is
  **INFERENCE_PROFILE-only**, so the bare id is not invocable and Converse answers 404.
  Measured in `us-west-2`: 43 of 108 foundation models are in that state, and they are the
  ones that matter (every Claude, Nova Pro/Micro, Llama 3.3/4, DeepSeek R1, Pixtral). The
  two halves also disagreed about which ID SPACE the form field held: the hardcoded table
  used `us.`-prefixed profile ids while discovery returned bare ids, in the same input.

  `/admin/bedrock/models` now queries `ListInferenceProfiles` **and**
  `ListFoundationModels` and returns only invocable targets: one entry per ACTIVE
  `SYSTEM_DEFINED` profile, plus the foundation models that support `ON_DEMAND` and are not
  already reachable through a profile. Each entry carries a `kind`
  (`inference_profile` / `foundation_model`), the `base_model_id` behind a profile, and
  `streaming`. Non-text models are filtered out; an *undeclared* modality is kept, because
  refusing it is how a brand-new model would silently disappear again.

  The console merges that live list with the curated table instead of replacing it: price
  and capabilities exist nowhere in Bedrock's APIs, and without a price `auto-cheapest`
  reads a model as free. Curated entries keep their price and caps, discovered ones are
  added at price 0 and flagged **no price** in the dropdown as well as at Add time. The
  list is seeded per region as soon as the Bedrock card opens, before any Role ARN is
  typed, and re-fetched from the customer's own account when one is supplied.

  **Upgrade note:** the cross-account role in the customer's account now needs
  `bedrock:ListInferenceProfiles` alongside `bedrock:InvokeModel` and
  `bedrock:ListFoundationModels`. A role that has not been updated still works — the
  profile half degrades to empty rather than failing the whole listing — but it will keep
  showing only the directly-invocable subset.
- **Three Portuguese strings shipped in the English console** (`' modelos encontrados'` in
  the BYO Bedrock fetch and the provider wizard, `usada em` in the credential-reuse list).
  All three escaped every gate: no accents to trip the heuristic, and not inside `_t()`.
- **`scripts/i18n-check.sh` check 3d only saw a literal that opened the value.**
  `el.textContent = 'prose'` was matched; `el.textContent = n + 'prose'` was not — which is
  exactly the shape of the strings above. It now matches a literal concatenated onto
  something, and strips same-line `className=` assignments first so a Tailwind class list
  is not mistaken for a sentence.

## [1.1.1] - 2026-09-15

A cache-tenancy fix that missed the 1.1.0 tag by twenty minutes, plus the guard that
keeps translations from regressing.

### Fixed

- **`cache_scope` was silently dropped on every save from the console.** `saveConfig()`
  writes an *allowlist* of fields back to the scope, and `cache_scope` was not in it — so
  a value set through the API was reverted to the inherited default by the next unrelated
  save, with no error and nothing on screen. For a field that decides whether one team can
  be served an answer produced from another team's prompt, that is a governance decision
  being undone quietly. Anyone on 1.1.0 has this bug; it is the reason for this release.
- **`/vendor/*` and `/assets/*` on a second, path-routing CDN.** The helper that wires an
  alternative console front door wrote its own bucket-policy statement for a distribution
  that `additional_oac_distribution_arns` already authorized. Two statements for the same
  distribution made `terraform plan` report drift on the frontend **permanently**: every
  apply removed the duplicate and the helper put it back. The authorization is now the
  Terraform variable's job alone, and the frontend plans clean.

### Added

- **Cache tenancy is now on screen**, not only in the API: a select in the Response cache
  card (team / app / deployment), synced from the config on every render so it cannot show
  one thing while the gateway enforces another.
- **`scripts/i18n-check.sh` runs in CI.** It could not be enabled before for a dull reason
  worth recording: it had unfixed findings, so it always exited non-zero — and a gate that
  always fails is a gate nobody turns on, which is how those findings survived. It passes
  clean at 1051/1051 keys, so a new untranslated string now fails the build instead of
  shipping. It also parses `console.js` with `node --check`, which is the only automated
  thing standing between a stray quote and a blank console.

### Changed

- Ten strings that rendered English inside a translated screen now have pt/es entries: two
  `data-i18n` attributes, two `_t()` literals, and six backend error messages (where the Go
  literal *is* the dictionary key, so a message with no entry stays English regardless of
  the language picked). The Response cache description was also corrected — it claimed "the
  key includes your org", which stopped being the whole truth once the key carried team and
  app.

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
[1.1.1]: https://github.com/aws-samples/sample-ai-platform-gateway/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/aws-samples/sample-ai-platform-gateway/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/aws-samples/sample-ai-platform-gateway/releases/tag/v1.0.0
