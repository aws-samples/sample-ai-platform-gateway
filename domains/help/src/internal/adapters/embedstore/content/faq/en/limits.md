## What this tab is for
Limits & Budget sets the caps that protect your invoice and stop one noisy team from hurting the others: rate limit (req/min) and a monthly budget in dollars, per scope (org, team or app).

## How to use it
Pick the scope, set the rate limit and budget from the steps, then choose what happens when the budget is hit: **warn**, **degrade** (forces the cheapest allowed model) or **block**. Changes take effect at the gateway within ~15s.

## Common questions
- **Org, team or app?** The limit counts in the scope that defined it: at the org level it applies to everything; on a team, only to that team.
- **Team or app — which one should I set?** Set it on the **team** by default: every app under that team shares the same cap, with no extra configuration. Only add a limit directly on an **app** when that one app needs a different cap than the rest of its team (e.g. a critical app that should never be throttled, or an experimental one with a tighter ceiling) — an app-level limit overrides the team's for that app only, the rest of the team is unaffected.
- **Is degrading safe?** It keeps the service up by switching to a cheaper allowed model instead of refusing the request.
