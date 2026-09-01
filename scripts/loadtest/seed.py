#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Seeds N teams x M apps x 1 key each, registers a Bedrock model in the org's
config, and turns on budget/rate-limits/cache/semantic-cache/alerts.

Usage:
  python3 seed.py --teams 5 --apps-per-team 5 --org XYZ_ORG
  python3 seed.py --teams 40 --apps-per-team 40   # full 30-50 x 30-50 run

Writes scripts/loadtest/state.json with every created team/app/key so the
load generator and the cleanup script can reuse them.
"""
import argparse
import json
import os
import random
import string
import sys
import time

from aiplat_client import AIPlatSession, load_config

STATE_PATH = os.path.join(os.path.dirname(__file__), "state.json")


def rand_suffix(n=4):
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=n))


def create_team(sess, cfg, org, name):
    url = f"{cfg['admin_api_url']}/admin/teams?org={org}"
    status, body = sess.post(url, {"display_name": name})
    return status, body


def create_app(sess, cfg, org, team_id, name):
    url = f"{cfg['admin_api_url']}/admin/apps?org={org}"
    status, body = sess.post(url, {"team": team_id, "display_name": name})
    return status, body


def issue_key(sess, cfg, org, team_id, app_id):
    url = f"{cfg['keyadmin_url']}/admin/keys"
    status, body = sess.post(url, {"org": org, "team": team_id, "app": app_id})
    return status, body


def put_config(sess, cfg, org, body, team=None, app=None):
    q = f"org={org}"
    if team:
        q += f"&team={team}"
    if app:
        q += f"&app={app}"
    url = f"{cfg['admin_api_url']}/admin/config?{q}"
    return sess.put(url, body)


def get_config(sess, cfg, org, team=None, app=None):
    q = f"org={org}"
    if team:
        q += f"&team={team}"
    if app:
        q += f"&app={app}"
    url = f"{cfg['admin_api_url']}/admin/config?{q}"
    return sess.get(url)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--teams", type=int, default=5)
    ap.add_argument("--apps-per-team", type=int, default=5)
    ap.add_argument("--org", default=None)
    ap.add_argument("--webhook-url", default=None, help="URL to receive alert firings")
    ap.add_argument("--skip-model-registration", action="store_true")
    args = ap.parse_args()

    cfg = load_config()
    org = args.org or cfg["org"]
    sess = AIPlatSession(cfg)
    claims = sess.login()
    print(f"[auth] logged in as {claims.get('email')} role={claims.get('custom:role')}")

    state = {"org": org, "teams": [], "started_at": time.time()}

    # 1. Register the Bedrock model + org-level config (budget, rate limits,
    #    cache, semantic cache, alerts) BEFORE creating teams, so every
    #    request issued later already has a valid route to call.
    if not args.skip_model_registration:
        model = cfg["default_bedrock_model"]
        org_cfg_body = {
            "auto_cheapest": False,
            "cache_ttl": 3600,
            "cache_key_mode": "canonical",
            "semantic_cache": True,
            "semantic_threshold": 0.88,
            "routing": {
                model["alias"]: {
                    "provider": "bedrock",
                    "provider_model_id": model["provider_model_id"],
                    "region": model["region"],
                    "capabilities": {"tool_use": False, "multimodal": False},
                }
            },
            "pricing": {
                model["alias"]: {
                    "input": model["price_input_per_1k"],
                    "output": model["price_output_per_1k"],
                }
            },
            "model_order": [model["alias"]],
            "default_model": model["alias"],
            "budget": {"limit_usd": 500, "action": "alert"},
            "rate_limits": {"requests_per_minute": 600, "tokens_per_minute": 2000000},
            "guardrails": {
                "mask_pii": True,
                "block_secrets": True,
                "block_injection": True,
                "no_store": False,
            },
            "alerts": {
                "webhook_url": args.webhook_url or "",
                "cost_usd": {"on": True, "threshold": 50},
                "latency_ms": {"on": True, "threshold": 5000},
                "cache_below": {"on": True, "threshold": 10},
                "error_rate": {"on": True, "threshold": 5},
                "provider_capacity": {"on": True, "threshold": 1},
                "slo_burn": {"on": True},
                "anomaly": {"on": True},
            },
        }
        status, body = put_config(sess, cfg, org, org_cfg_body)
        print(f"[config] PUT /admin/config?org={org} -> {status}")
        if status >= 300:
            print(json.dumps(body, indent=2))
            sys.exit(1)
        state["org_config"] = org_cfg_body

    # 2. Teams + apps + keys.
    #
    # KNOWN BUG (found by this harness): governance's team/app registry is a
    # SINGLE DynamoDB item per org (pk TEAMS#<org> — see config-api/main.go
    # orgTreeKey), written via a non-atomic read-modify-write (readOrgTree ->
    # pure AddApp/AddTeam -> writeOrgTree PutItem, no ConditionExpression / no
    # optimistic lock on a version field). Two concurrent POST /admin/apps (even
    # for DIFFERENT teams) race: the second request's writeOrgTree overwrites
    # the first request's app with a tree read before the first write landed.
    # keyadmin's readOrgTree-based existence check then reports the clobbered
    # app as nonexistent ("app does not exist — create the app under Teams &
    # Apps before issuing the key"), even though POST /admin/apps returned 200.
    # This is a REAL correctness bug for any multi-actor org, not a test
    # artifact — see the analysis writeup for the suggested fix (optimistic
    # concurrency via a version/updated_at CAS, or a per-team item instead of
    # one item per org). Worked around here by creating teams/apps/keys
    # strictly SEQUENTIALLY: slower, but doesn't corrupt the registry.
    for ti in range(args.teams):
        team_name = f"Team {ti+1:02d} {rand_suffix()}"
        status, body = create_team(sess, cfg, org, team_name)
        if status >= 300:
            print(f"[team] FAILED {team_name}: {status} {body}")
            continue
        team_id = body["id"]
        print(f"[team] {ti+1}/{args.teams} created id={team_id} name={team_name}")
        team_entry = {"id": team_id, "name": team_name, "apps": []}

        for ai in range(args.apps_per_team):
            app_name = f"App {ai+1:02d} {rand_suffix()}"
            status, body = create_app(sess, cfg, org, team_id, app_name)
            if status >= 300:
                print(f"  [app] FAILED create {app_name}: {status} {body}")
                continue
            app_id = body["id"]
            status, kbody = issue_key(sess, cfg, org, team_id, app_id)
            if status >= 300:
                print(f"  [key] FAILED for app={app_id}: {status} {kbody}")
                continue
            team_entry["apps"].append({"id": app_id, "name": app_name, "api_key": kbody["api_key"]})

        print(f"  -> {len(team_entry['apps'])}/{args.apps_per_team} apps+keys created")
        state["teams"].append(team_entry)
        with open(STATE_PATH, "w") as f:
            json.dump(state, f, indent=2)

    total_apps = sum(len(t["apps"]) for t in state["teams"])
    print(f"\n[done] {len(state['teams'])} teams, {total_apps} apps/keys created.")
    print(f"[done] state written to {STATE_PATH}")


if __name__ == "__main__":
    main()
