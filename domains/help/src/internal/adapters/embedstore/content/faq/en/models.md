## What this tab is for
Models & Routing is where you connect providers and models, set the priority order (default + fallback), and turn on cache and auto-cheapest. Every model is a **route**: an alias your code calls that points at a provider — swapping the provider behind it does not change a line of your app.

## How to use it
On the **Models** sub-tab, add providers and turn each model on or off (tags 🌐 external / 🏠 internal). Under **Routing**, drag to order the models that are on. Under **Settings**, enable auto-cheapest and the caches. Price comes pre-filled from the public list; if you have a discount, edit it to yours.

## Common questions
- **External vs internal?** External = SaaS provider (including Bedrock, which runs in your own account). Internal = your own endpoint (self-hosted).
- **Does auto-cheapest ignore my order?** When it is on, the attempt order becomes price (cheapest → next), still respecting eligibility (tool use, images, context window).

## Exact cache vs semantic cache

These are two different mechanisms, and the difference matters.

**Exact cache** stores the answer and returns it when the very same question comes back. The saving is **verified**: the provider call simply did not happen. There is no risk.

**Semantic cache** goes further and returns the answer to a *similar* question, comparing meaning. It changes both the cost and the risk profile:

- **Latency cost:** it adds ~60 ms on the requests that **miss**, because the question has to be turned into a vector first.
- **Upside:** every hit avoids **100% of the cost** of that call. How much that cuts your invoice depends on your hit rate, which you track in **ROI & Savings**.
- **Risk:** the answer is **approximate, not identical**. It admits false positives.

That is why it ships off by default and why its savings appear separately from verified savings in ROI.

### When NOT to use semantic cache
If your questions are distinguished by **fine detail**, the mechanism tends to hurt more than it helps. Examples where the textual difference is small and the correct answer is very different: a question about one team vs another, one period vs another, one feature vs a similar one.

One protection is automatic: questions with **different numbers never match** each other (60 and 600 will not be confused), because meaning-based comparison is at its weakest precisely with quantities.

### Strictness (threshold)
Strictness sets how similar two questions must be to match. Higher = fewer hits, less risk. Lower = more hits, more chance of a wrong approximate answer. The default is deliberately conservative.
