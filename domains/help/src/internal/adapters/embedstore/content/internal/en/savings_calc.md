# Savings calculation — how the ROI ledger is assembled

> AIPlat internal. Explains where each `saved_usd` comes from and why we separate by strength of evidence.

## Two classes of saving
- **Verified (`saved_verified_usd`)** — depends on no counterfactual. Today that is the **cache**: the same model would have been called and billed; because we served from cache, the avoided cost is **observable** (that model's price × the tokens the answer would have used). `provider_prompt_cache` also lands here (the provider charged less for the prefix). This is the defensible basis for gain-share.
- **Counterfactual (`saved_counterfactual_usd`)** — a **model swap** happened: served ≠ requested. It is measured against the **model the customer asked for**. It is real money, but it only holds if the request reflected the real intent. Reasons: `auto_cheapest`, `fallback`, `budget_degrade`.

`saved_verified + saved_counterfactual == saved_usd`.

## Auto-cheapest (the calculation that raises the most questions)
1. The router receives a request asking for model M (or the default model from the order).
2. With auto-cheapest on, it picks the **cheapest eligible** model from the enabled list (eligibility = tool use, multimodal, context window). Call it C.
3. It serves with C. The real cost is `cost(C) = tokens × price(C)`.
4. The **baseline** is the **requested** model M: `RequestedCostUSD = actual_tokens × price(M)` — what M would have cost **on the same output tokens**.
5. `saved = max(0, RequestedCostUSD − cost(C))`, with `savings_reason = auto_cheapest`. If there was no swap (C == M), `saved = 0` — there is no "phantom saving".

The baseline (`requested_model` / `requested_cost_usd`) is **emitted by the router** and **persisted** in the Usage_Record, which is what makes the counterfactual **auditable** after the fact.

## Fallback and budget_degrade
- `fallback`: the requested model's provider failed; we served the next in the chain. Baseline = requested, same formula.
- `budget_degrade`: the budget was exceeded with action `degrade`; we forced the cheapest allowed model. Baseline = requested.

## Credit ≠ savings
`credit_usd` / `cash_usd` partition the **same** `cost_usd` by pocket (provider credit burned vs actual outlay). **Adding them to `cost` would double-count.** Burned credit is consumed balance, not savings — which is why "Which pocket paid" shows it separately, so verified savings are not inflated.

## Price: list vs contract
`cost_list_price_usd` / `cost_contract_price_usd` mark the provenance of the price. If the customer is on list price and has an unregistered discount, cost and savings both sit **above** the real figures — `list_price_pct` exposes how much of the period carries that bias.
