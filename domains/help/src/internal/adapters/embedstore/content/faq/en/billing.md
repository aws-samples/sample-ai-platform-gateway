## What this tab is for
Plan & Billing shows the current month's metering (read from real history), the estimated invoice and the plan switch. LLM spend is yours (your own credential); the platform charges the subscription and, on the plans that have it, a fraction of the verified saving.

## How to use it
Look at the consumption cards and the month's estimated invoice. To change plans, pick the new tier — the limits (seats, rate limit, budget) take effect as your org config.

## Common questions
- **Is the invoice a real charge?** Not at this stage — it is an estimate from measured usage.
- **What changes when I move up a plan?** Member seats, limits and the access model (per user on Pro, per team on Business).

## Why the credit balance is an estimate

Provider credit (AWS Activate, Google Cloud, Azure) is **balance**, not a discount: the price per token does not change, only which pocket pays. While there is balance, routing prefers the covered provider — burning the credit before it expires is the right move.

The balance shown here is **estimated**, and it is a **lower bound** on real consumption. The reason is simple: we only count what went through the gateway, and provider credit is also consumed by everything you use outside the platform (storage, machines, other services).

**What to do:** check the balance on your provider invoice and correct the value on screen. The manual correction becomes the basis of the calculation, replacing the originally declared amount.

When the credit expires, the gateway goes back to deciding on real money and your budget applies normally again.
