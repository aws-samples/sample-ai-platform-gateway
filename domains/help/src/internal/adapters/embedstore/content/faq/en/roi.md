## What this tab is for
ROI & Savings shows how much the gateway avoided spending and why, separated by strength of evidence: **verified** savings (cache — the same model cost less, and that is observable) and **counterfactual** savings (we served a model other than the one requested, and compared against what the request would have cost).

## How to use it
The cards total savings per mechanism (cache, auto-cheapest, fallback, budget degrade). The charts show each mechanism over time plus the consolidated figure. "Which pocket paid" separates provider credit (balance burned) from real savings — credit is not savings.

## Common questions
- **Which savings can I take seriously?** The verified kind (cache) rests on no assumption. The counterfactual kind is real money, but it only holds if the requested model was the real intent.
- **Why do the savings look high?** If the price is list price and you have a contract discount, enter your price in Models & Routing — otherwise both cost and savings sit above the real figures.
