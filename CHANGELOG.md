# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: the client-facing contract is the **OpenAI dialect**, so `MAJOR` is
reserved for a change that breaks a caller's request or response. Changes that need action
from an *operator* — Terraform variable defaults, deployed object layout — are `MINOR` with
an **Upgrade notes** section, which is where they are called out.

## [Unreleased]

## [1.4.0] - 2026-09-19

An optional second model transport: the router can now reach Anthropic models through an
Amazon Bedrock AgentCore Gateway, selected per route with the `bedrock_gateway` provider,
alongside the existing direct `bedrock` provider. It is off by default and changes nothing
for a deployment that does not enable it.

### Added

- **AgentCore Gateway transport (`bedrock_gateway` provider).** A route can now be served
  through an Amazon Bedrock AgentCore Gateway instead of calling Bedrock directly. Terraform
  creates the gateway with two targets — a `bedrock-mantle` target for versionless Anthropic
  model ids and a `bedrock-runtime` target for the geo-prefixed inference profiles — and the
  router picks the target from the model id. Inbound auth is `AWS_IAM`, so the router signs
  with the deployment's own credentials and no bearer token is stored anywhere. Every route
  on the existing `bedrock` provider is unchanged.

### Upgrade notes

- **New and optional; off unless you turn it on.** The module variable
  `agentcore_gateway_enabled` defaults to `false`, so a deployment that never sets it is
  byte-identical to one without this feature. Enabling it also requires wiring an
  `aws.agentcore` provider alias and setting `agentcore_gateway_region` — deliberately
  independent of the deployment region, because the model catalog behind the gateway differs
  by region. The bundled `poc` environment opts in.
- **The gateway adds its own per-request charge** on top of the model call, and it is an
  alternative door to models the `bedrock` provider already reaches (it reaches fewer of
  them), so turning it on is a deliberate choice rather than a default.

## [1.3.0] - 2026-09-16

Reasoning models are now first-class: their chain of thought reaches the client, the silence
while they think no longer looks like a dropped request, and a request can no longer run to the
platform ceiling and take its own accounting down with it. The sampling parameters a client
sends now reach the provider at all, which they previously did not.

This release also closes a real credential-exposure hole found by a functional audit of the
console and every admin route against what each promises: a member with no write permission at
all could make the platform hand over the org's stored provider API key. See **Security** below
before anything else in this entry.

### Upgrade notes

Read this one before deploying: it changes behaviour on traffic you already have.

- **`max_tokens` and `temperature` are now ENFORCED. They were silently ignored.** Both were
  parsed from the request, used for the cache key and the routing decision, and then dropped
  before the provider call — `ports.InvokeInput` even carried a `MaxOutputTokens` field that no
  adapter read. Measured on the reference deployment before the fix: a request sending
  `max_tokens: 400` came back with **560** completion tokens. After: `max_tokens: 64` returns
  exactly 64 with `finish_reason: "max_tokens"`.

  **What changes for you:** any caller that has been sending a `max_tokens` smaller than the
  length it actually relies on will start receiving truncated answers — and it will look like a
  regression, when it is the parameter finally being honoured. Grep your callers for
  `max_tokens` before deploying. Cost moves the other way: requests that set a low ceiling get
  cheaper. `temperature: 0` now genuinely produces deterministic output.

- **Anthropic answers were capped at 1024 tokens, always.** The adapter hardcoded
  `max_tokens: 1024` with no way for a caller to change it, so every longer answer was
  truncated there. The default is now 4096 and a client value overrides it. Answers on that
  route may get longer, and correspondingly more expensive.

- **Cache keys change for requests that send `top_p`, `stop` or `reasoning_effort`.** Those
  three now take part in the key, which is required for correctness — otherwise a `top_p: 0.1`
  request could be served the answer generated at `top_p: 1.0`. Requests sending none of them
  keep their existing keys, so a warm cache is not lost.

### Fixed

- **Bedrock reasoning was being discarded, in both the streaming and the buffered path.** The
  content-block type switches handled text and tool use and had **no default arm**, so
  `reasoningContent` was dropped without a trace. Streaming showed the symptom backwards: an
  extended-thinking model was sending bytes the entire time it thought and the adapter threw
  them away, so "the model goes quiet for minutes before answering" was produced here, not by
  the provider or the transport. The buffered path hid it better — the answer still arrived,
  while the thinking **signature** that lets a reasoning chain continue on a later turn was
  discarded on every request. Both switches now have a `default` arm that warns once per
  stream, so the next member the SDK adds shows up as a log line instead of as missing data.
- **The native Anthropic adapter was speaking the OpenAI dialect to the Messages API.** It sent
  `tool_calls`, `tool_call_id`, `name` and the client's raw `content` verbatim — none of which
  exist in that API. Anthropic ignores fields it does not recognise, so nothing failed: a
  request carrying `tools` got a prose answer and no tool call, a tool result was invisible (so
  a tool conversation could not progress past the first call), and a multimodal message arrived
  as an OpenAI `image_url` part the model cannot read. `in.Tools` was never read at all.
  Messages now translate into native blocks — `text`, `image`, `tool_use`, `tool_result` —
  tools go out as `input_schema` declarations, and `tool_use` comes back as a tool call on both
  the buffered and streaming paths (the stream fragments arguments across `input_json_delta`
  events, so they are accumulated). `stop_reason` is also read now; without it a tool call
  looked like a completed answer.
- **The native Gemini adapter projected only `m.Text`.** Tools, images and function RESULTS
  were all dropped, with the same silent-success signature. Messages now become `parts` with
  `inlineData` for images and `functionCall`/`functionResponse` for tool turns, and tools go out
  as `functionDeclarations`. Two Gemini-specific traps are handled: it identifies a function
  response by NAME while the OpenAI dialect uses `tool_call_id` (the name is recovered from the
  assistant turn that requested it), and it REJECTS JSON Schema keywords other providers accept
  (`$schema`, `additionalProperties`, `$ref`, …), which is what makes a schema generated by the
  customer's own tooling come back as a 400 naming a field they never wrote — those are now
  stripped. Gemini returns no call id, so a stable one is synthesized from name and position,
  because the client echoes it back on the result.
- **The wire goldens could not have caught any of the above.** They passed no tools at all, and
  their image was the placeholder `data:x`, which cannot be decoded — so `Images` came out empty
  and the image path was never exercised. They now carry a real 1×1 PNG and a tool whose schema
  includes the keywords Gemini rejects, which is what makes the three call sites (send tools,
  send images, sanitize the schema) covered rather than merely implemented.
- **The Anthropic buffered path would have returned an EMPTY answer for every reasoning
  request.** It read `content[0].text`, which is correct for a plain answer — a plain answer has
  exactly one text block — so it stayed correct for as long as thinking could not be requested.
  With thinking enabled the response is `[{type:"thinking"},{type:"text"}]` and a thinking block
  carries no `text` field. Every block is now dispatched by type, which also fixes a
  multi-block answer losing everything after the first.
- **The Gemini buffered path would have returned the model's reasoning AS the answer.** It read
  `parts[0].text` and ignored the `thought` flag; once `thinkingConfig` is sent with
  `includeThoughts`, part 0 IS the thinking. Two bugs in one line: it also truncated any
  multi-part answer to its first part, which the streaming path's own comment already called
  out.
- **`reasoning_content` never reached a streaming client on providers without a native streaming
  API** (every OpenAI-compatible route, since that adapter has no `OpenStream`). The fallback
  that slices a complete answer into frames emitted only the answer, so the reasoning was
  parsed, billed and written to the ledger and then never shown. It now emits reasoning frames
  first, matching the order a native reasoning stream produces. The same fix covers replaying a
  cached answer that was produced with thinking — both cache-hit paths shared a duplicated
  extraction that read only `content`.
- **A streamed reasoning answer was CACHED without its reasoning.** The streaming path stored
  only `role` and `content`, so the next caller of the same prompt was served an answer that
  looked as though the model had never reasoned — the first caller paid for reasoning that was
  then thrown away on every replay. Found while re-verifying against the live deployment: the
  probe's own earlier answers were coming back from cache with every reasoning field empty.
- **Data race on the token counters of an abandoned stream.** The stream pump read the
  adapter's accumulated `Result` after giving up, while its reader goroutine could still be
  blocked inside that adapter — and cancelling the pump's deadline does not cancel the
  provider's HTTP stream, since that was opened with the request context, so the read
  completes and mutates the counters underneath. The `Result` is now snapshot by the reader
  goroutine and published under a mutex, which leaves the adapter touched by exactly one
  goroutine. Caught by `go test -race`; the failure mode without the detector is worse than a
  crash, because a torn read of the token counters is a wrong number in the ledger that
  nothing reports.

### Added

- **`reasoning_content` on the response**, as a field of its own on the streaming delta and on
  the buffered message — never merged into `content`, because every OpenAI SDK concatenates
  `delta.content` and merging would print the model's reasoning inside the answer shown to an
  end user. Clients that do not know the field ignore it.
- **SSE keepalive during silence, on by default (15 s).** Idle timeouts count bytes, not
  progress, so a proxy at 60 s or an API Gateway idle cut at 5 minutes kills a request that is
  working fine while the model thinks. A `: ping` **comment** frame is sent instead — the SSE
  spec tells clients to ignore it, and it is excluded from every counter, so it is never
  mistaken for content and never appears as cost. Per scope via `sse_keepalive_seconds`: unset
  (0) takes the default, a **negative** value disables it. Zero cannot mean "off" because an
  int with `omitempty` makes "off" and "never configured" the same bytes on the wire, and
  reading zero as off would leave every existing deployment unprotected.
- **`request_timeout_ms` per scope**, bounding the generation phase so the closing frames and
  the usage record still get written. Clamped against the **runtime's own** deadline read from
  the request context rather than a configured constant, so raising the Lambda timeout raises
  the ceiling automatically and a scope can only ever narrow it — the same rule
  `allowed_models` follows. Without it, a request that reaches the platform ceiling is cut
  mid-answer and the tokens the provider already billed are never accounted for.
- **`reasoning_chars` and `reasoning_redacted` in the usage record and the `aiplat` block**,
  present only when the model actually reasoned. Characters and not tokens on purpose: Bedrock
  reports input/output/total only, so reasoning tokens are already inside `tokens_out` —
  already billed, already attributed. What was missing is the split, and converting characters
  to tokens would be a guess presented as a measurement. `reasoning_redacted` marks thinking
  the provider encrypted, keeping "did not think" distinguishable from "not shown to us".
- Both new fields have console controls under **Long answers** on the Models → Settings tab.
  They were added to the `saveConfig` allowlist *and* given controls in the same change: that
  list copies from the effective config, so an allowlisted key with no control would have
  written the value inherited from the parent scope into the child as a local declaration,
  silently pinning what used to follow the org.
- **`top_p` and `stop` on the request**, the two sampling parameters the dialect allows and this
  gateway did not accept. `stop` takes a string or an array of strings, as the dialect permits;
  an empty entry is dropped, because on some providers it matches immediately and truncates the
  answer to nothing.
- **`reasoning_effort` (`none` | `low` | `medium` | `high`)**, resolved once in the gateway into
  the token budget each provider requires — Bedrock and Anthropic `thinking.budget_tokens`,
  Gemini `thinkingConfig`, and the word itself passed through to an OpenAI-compatible upstream.
  Four adapters each interpreting "high" would be four different products under one name.
  `none` is an explicit *do not think*, kept distinct from an absent field because the providers
  that can honour it need to be told.
- **`capabilities.reasoning` per model**, with a toggle on the Models tab. A request that asks
  to think is routed to a model that can, and an undeclared model counts as incapable — the same
  conservative default tool use uses. Nothing detects this for you: no provider API reports
  whether a model supports extended thinking, and guessing wrong makes the provider reject the
  request. The thinking budget also counts against the context-window check, because thinking
  tokens are output tokens.
- **`max_thinking_tokens`**, the operator's ceiling on what one request may spend on thinking
  (default 8192, hard maximum 32768, console control on the Models → Settings tab). Thinking is
  billed as output, so without a ceiling one caller can multiply the cost of every request with
  nothing changing on the operator's side. Over-ceiling requests are served with the budget
  reduced and `reasoning_clamped` recorded — clamping without recording would be the dishonest
  half of that choice.
- **`reasoning_effort`, `thinking_budget_tokens` and `reasoning_clamped` in the usage record and
  the `aiplat` block.** Effort and budget are two fields because they can legitimately disagree,
  and that disagreement is exactly what an operator needs to see when a ceiling is doing its
  job.
- **Anthropic and Gemini now expose reasoning too.** The shared SSE helper returned a bare
  string, so a dialect had nowhere to put a thinking delta and could only drop it or blend it
  into the answer. It returns a `ports.Chunk` now, which is what lets `thinking_delta`,
  `signature_delta` and `redacted_thinking` (Anthropic) and `parts[].thought` /
  `thoughtSignature` (Gemini) reach the client through the same path Bedrock already used.
  Gemini's `thoughtsTokenCount` is deliberately NOT added to the totals — it is already inside
  `candidatesTokenCount`, and adding it would double-charge the thinking.
- OpenAI-compatible upstreams that return reasoning are now read: both `reasoning_content`
  (DeepSeek, TrueFoundry) and `reasoning` (OpenRouter), since the dialect never converged on one
  name and reading only one silently drops it for half the providers a customer might point at.

### Changed

- **Console: inline markup in translated strings renders again.** `applyStaticI18n` assigned
  every `data-i18n` string with `textContent`, so authored `<b>`, `<code>`, `<span>` and
  `&nbsp;` printed as literal tags — the Models tab read *"The &lt;b&gt;toggle&lt;/b&gt; says
  what the gateway may use"*. It survived because the markup is authored correctly in the page
  and only clobbered when that function first runs. It now uses `innerHTML` for strings
  containing markup, which is safe on this path and only this path: both the key and the value
  are static and ours (an attribute in `console.html`, a value from the dictionaries in
  `console.js`). Values interpolated from data still go through `esc()`.
- The Bedrock buffered call gained a client interface (`converseAPI`), mirroring the one the
  streaming path already had. Without it there was no way to test what `callBedrock` actually
  put on the wire — reintroducing "drop every inference parameter" left the suite green, which
  is the same structural gap that let the reasoning block go unhandled in that file.

### Security

- **`GET /admin/provider/models` let any authenticated member exfiltrate the org's stored
  provider API key.** The route's only check was that the caller belonged to the org — the
  same gate as a read — but it makes the platform send that org's vault credential, as an
  `Authorization`/`x-api-key`/`?key=` header, to whatever `base_url` the request names. A
  `dev` or `billing` member, neither of whom can write anything else in the console, could
  point `base_url` at a host they control and receive the org's real OpenAI/Anthropic/Gemini
  key in the resulting request. Every other route that touches a credential (`POST
  /admin/secrets`, `PUT /admin/config`) already required `owner`/`admin`; this one did not.

  Fixed with two independent layers: the route now requires `owner`/`admin`, matching
  `/admin/secrets`; and `base_url` must match a `base_url` already declared on one of the
  org's own routes (`orgDeclaresBaseURL`), so even an admin cannot redirect the credential to
  an arbitrary destination. Found and verified with a Playwright-driven audit of the console
  against every role, plus characterization tests that fail against the pre-fix handler and
  pass with it. No upgrade action needed — this only removes access a role never should have
  had — but audit your logs for `unauthorized_provider_models_access_attempt` /
  `provider_models_undeclared_base_url` entries if you suspect the route was probed.

### Fixed

- **`GET /admin/keys` silently returned a partial key list past ~1&nbsp;MB of table
  content.** It issued a single unpaginated `Scan` with a `FilterExpression`, and DynamoDB
  applies the filter *after* reading a page — so an org whose api-keys table exceeded the
  page boundary saw a truncated list presented as complete: the Overview "API Keys" card and
  the Teams & Apps key counts under-reported, and an existing key could look revoked. It is
  now the only route in the repo that was missing the `LastEvaluatedKey` loop every other
  paginated read already follows; fixed to match.
- **The console only hid gated panels from the sidebar, not from every way to reach
  them.** `applyRoleNav()` hid the sidebar's own buttons for `dev`/`billing`, but `show(v)` —
  the single primitive every navigation path funnels through, including the Overview
  health-cards — never checked the role. A `dev` clicking the "Cost & Budget" card landed on a
  fully interactive Limits & Budget screen and only discovered the restriction after a 403 on
  Save. `show(v)` now enforces the same allowlist for every caller, and a gated Overview card
  drops its click/keyboard affordances (`aria-disabled`) while still showing its data. This
  was UI-only: the backend authorization was already correct and is unchanged.
- **Six strings shipped in Portuguese inside the English console**, invisible to
  `scripts/i18n-check.sh` because they were single words with no accent (`arquivado`,
  `carregando…`) or sat inside a template literal the existing checks do not parse
  (`membros`, `chave(s)`, an unwrapped `<option>`, an unwrapped placeholder). Rewritten through
  `_t()`, with the missing dictionary keys added to both `pt` and `es`. Added check 7 to
  `scripts/i18n-check.sh`: a curated marker sweep over `console.js` outside the dictionary
  blocks, so a recurrence of this specific defect fails the build instead of shipping quietly.

### Changed

- **The local demo fixtures (`demo/fixtures.mjs`, `demo/server.mjs`) now match the real API
  response shapes.** They were missing `status`/`org` on keys, `by_upstream` and six fields on
  every `usage-api` provider breakdown row, and `requested_cost_usd`/`served_model_id`/
  `status`/`cache_hit` on log records — so those console code paths were never exercised
  offline and never appeared in a generated screenshot. `server.mjs` also now scales every
  breakdown table when slicing a short window, not only the summary cards, so a 7-day view is
  internally consistent instead of showing 30 days of breakdown next to 7 days of totals.
  Local/offline only; nothing here is deployed.

All of the above were found by a from-scratch functional audit: every documented promise in
the README and the console's own copy checked against the running code, the console driven in
a real browser under every role, and the live control-plane APIs probed read-only. Full report
kept out of the published tree (`.kiro/specs/audit-fable/`, gitignored) — this entry and the
Security note above are the parts of it that belong in a changelog.

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
