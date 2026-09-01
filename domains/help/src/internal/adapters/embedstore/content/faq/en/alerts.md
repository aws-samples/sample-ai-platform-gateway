## What this tab is for
Alerts defines rules evaluated against real usage (spend, latency, low cache, error rate, provider capacity) and a webhook to receive the notice when a rule fires.

## How to use it
Turn on the rules that matter and set each threshold. Enter a webhook (your own URL) to receive the POST. An evaluator runs server-side every 15 min and delivers to the webhook at most once per rule per day.

## Common questions
- **Is the plan-consumption rule delivered?** It is evaluated live on this screen, but it is not delivered by webhook yet.
- **Why did it not fire?** Error rules have a volume floor per window, to avoid noise when there are few requests.

## How delivery works (and what is not delivered yet)

An evaluator runs server-side every **15 minutes**, checks the rules that are on, and POSTs to your webhook when one fires. Firing is capped at **once per rule per day** (cooldown), so a continuous problem does not turn into a flood of notices.

**Delivered by webhook:** monthly spend, average latency, low cache, error rate, provider capacity, error budget burn (SLO) and anomaly versus your own history.

**Evaluated live on this screen only:** plan quota consumption. It depends on the plan catalogue, which belongs to another domain, and is not delivered by webhook yet.

## An alert can fire and never arrive

If your webhook is down or refuses the call, the notice is lost — and it is **not resent**, because the day's cooldown has already been consumed.

That is why the history exists under **Logs → Alerts**: there you see every firing, the value that triggered it and whether delivery worked. If it says "not delivered", check the webhook URL here. We keep only the host of the address, never the full URL, because it usually carries a token.
