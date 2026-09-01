# AIPlat load-test harness

Seeds teams/apps/keys, registers a real Bedrock model, turns on
budget/rate-limits/cache/semantic-cache/guardrails/alerts, fires real
chat/completions requests through the gateway, and analyzes the results for
errors, mocked-looking values, and whether every governance feature actually
works end to end.

Stdlib Python 3 + `boto3` only (no `requests`), so it runs with whatever
Python is already on the machine.

## Setup

```bash
cp .env.example .env
# edit .env with AIPLAT_USER / AIPLAT_PASS
```

`.env` is gitignored (matches `*.env` in the repo root `.gitignore`).
`config.json` has the live API endpoints, Cognito pool/client, the seed org
(`XYZ_ORG`) and the Bedrock model to register — no secrets, safe to commit,
but re-check it if the deployment's endpoints ever change (`terraform output`
in each domain's `envs/poc`).

## Run it directly

```bash
# 1. Seed N teams x M apps, each with its own API key. Also registers a
#    Bedrock model + org config (budget, rate limits, cache, semantic cache,
#    guardrails, alerts) on the FIRST run — pass --skip-model-registration on
#    later runs against the same org if you don't want to touch that config.
python3 seed.py --teams 30 --apps-per-team 30

# 2. Fire real requests through the gateway (real Bedrock spend, small).
python3 load.py --requests 150 --workers 6

# 3. Analyze results.json (+ pull live /usage/summary, /usage/alerts,
#    /audit/records if --live-summary).
python3 analyze.py --live-summary
```

Every script re-reads `state.json` (written by `seed.py`) so you can seed
once and run `load.py` many times against the same teams/apps/keys.

## Run it via the webhook

```bash
python3 webhook_server.py --port 8787 &

curl -X POST http://localhost:8787/run -H 'content-type: application/json' -d '{
  "teams": 10, "apps_per_team": 10, "requests": 200,
  "targets": ["cost", "latency", "cache", "semantic_cache", "guardrails", "alerts"],
  "org": "XYZ_ORG"
}'
# -> {"run_id": "20260818-123456", "run_dir": "...", "status": "started"}

curl http://localhost:8787/status/20260818-123456
```

Each webhook-triggered run is fully logged under `runs/<run_id>/` (seed.log,
load.log, analyze.log, results.json, report.md, params.json) so repeated
triggers never overwrite each other's evidence. Set `"skip_seed": true` to
reuse the existing `state.json` instead of reseeding.

`targets` only shapes which prompt patterns `load.py` uses (repeats for
cache, paraphrases for semantic cache, PII/injection/secret probes for
guardrails) — it does not (yet) selectively skip parts of the seed. Alert
delivery to a real webhook URL still requires a publicly reachable HTTPS
endpoint (this local server is a trigger, not that receiver — see the
docstring in `webhook_server.py`).

## Known issues found by this harness

See `.kiro/steering/aiplat-loadtest-findings.md` (load manually with
`#aiplat-loadtest-findings`) for the running list of confirmed bugs, what was
fixed, and what's still open — update it every time you re-run this and find
or fix something.

## Files

- `aiplat_client.py` — Cognito login (USER_PASSWORD_AUTH) + a small HTTP
  client (stdlib `urllib`, retries, JSON in/out) shared by every script.
- `config.json` — live endpoints/org/model, safe to commit.
- `.env` / `.env.example` — credentials, `.env` is gitignored.
- `seed.py` — creates teams/apps/keys + org config. **Sequential on purpose**
  (see the comment block in the file) because of the concurrency bug in
  `.kiro/steering/aiplat-loadtest-findings.md` item #2.
- `load.py` — fires real chat/completions requests, writes `results.json`.
- `analyze.py` — reads `results.json` (+ live API reads), writes `report.md`.
- `webhook_server.py` — local HTTP trigger for a customizable run; logs to
  `runs/<timestamp>/`.
- `smoke_chat.py` — one-off single request against the first key in
  `state.json`, for a fast sanity check without running the full load.py.
