#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Analyzes results.json (from load.py) plus live reads from usage-api,
audit-api and CloudWatch to surface: errors, cost/latency stats, cache
effectiveness, alert firings, guardrail hits, and anything that looks
mocked/inconsistent (e.g. cost always exactly 0, latency suspiciously
constant, savings never attributed).

Usage:
  python3 analyze.py                  # analyze the last load.py run
  python3 analyze.py --live-summary   # also pull /usage/summary from the API
"""
import argparse
import json
import os
import statistics as stats

from aiplat_client import AIPlatSession, load_config

RESULTS_PATH = os.path.join(os.path.dirname(__file__), "results.json")
STATE_PATH = os.path.join(os.path.dirname(__file__), "state.json")
REPORT_PATH = os.path.join(os.path.dirname(__file__), "report.md")


def load_results():
    with open(RESULTS_PATH) as f:
        return json.load(f)


def analyze_local(data):
    results = data["results"]
    ok = [r for r in results if r["status"] == 200]
    errs = [r for r in results if r["status"] != 200]

    findings = []

    # --- error breakdown ---
    err_by_status = {}
    for r in errs:
        err_by_status.setdefault(r["status"], []).append(r)
    for status, rs in sorted(err_by_status.items()):
        sample = rs[0]["body"]
        findings.append(f"- {len(rs)}x HTTP {status}: sample body = {json.dumps(sample)[:200]}")

    # --- suspiciously mocked signals ---
    costs = [r["body"].get("aiplat", {}).get("estimated_cost_usd") for r in ok]
    lats = [r["body"].get("aiplat", {}).get("latency_ms") for r in ok if r["body"].get("aiplat", {}).get("latency_ms") is not None]
    if costs and all(c == 0 for c in costs):
        findings.append("- SUSPECT: every request has estimated_cost_usd == 0 (pricing not registered? check config.pricing)")
    if costs and len(set(costs)) == 1 and costs[0] not in (0,):
        findings.append(f"- SUSPECT: every request has the IDENTICAL cost {costs[0]} (possibly hardcoded, not computed from tokens)")
    if lats and len(set(lats)) == 1:
        findings.append(f"- SUSPECT: every request reports the IDENTICAL server latency_ms={lats[0]} (possibly not measured per-request)")

    cache_hits = [r for r in ok if r["body"].get("aiplat", {}).get("cache_hit")]
    repeat_sent = [r for r in results if r.get("prompt_kind") == "repeat_exact"]
    if repeat_sent and not cache_hits:
        findings.append(
            f"- SUSPECT: {len(repeat_sent)} requests reused an exact prior prompt but ZERO cache_hit=true in responses "
            "(cache_ttl too short? cache_key_mode mismatch? cache not actually wired for this route?)"
        )

    semantic_variant = [r for r in results if r.get("prompt_kind") == "semantic_variant"]
    savings_semantic = [
        r for r in ok
        if r["body"].get("aiplat", {}).get("savings_reason") == "semantic_cache"
    ]
    if semantic_variant and not savings_semantic:
        findings.append(
            f"- NOTE: {len(semantic_variant)} paraphrased-prompt requests sent, 0 got savings_reason=semantic_cache "
            "(expected if semantic_threshold=0.88 is stricter than the paraphrase similarity — verify by lowering threshold, not necessarily a bug)"
        )

    guardrail = [r for r in results if r.get("prompt_kind") == "guardrail_probe"]
    guardrail_blocked = [r for r in guardrail if r["status"] in (400, 403)]
    if guardrail and len(guardrail_blocked) < len(guardrail):
        findings.append(
            f"- SUSPECT: {len(guardrail) - len(guardrail_blocked)}/{len(guardrail)} guardrail-probe prompts "
            "(PII/secret/injection) were NOT blocked — check mask_pii/block_secrets/block_injection config"
        )

    # --- basic stats ---
    if lats:
        p50 = stats.median(lats)
        p95 = sorted(lats)[int(len(lats) * 0.95) - 1] if len(lats) > 1 else lats[0]
        findings.append(f"- latency_ms: p50={p50:.0f} p95={p95:.0f} min={min(lats)} max={max(lats)}")

    reasons = {}
    for r in ok:
        reason = r["body"].get("aiplat", {}).get("savings_reason") or "none"
        reasons[reason] = reasons.get(reason, 0) + 1
    findings.append(f"- savings_reason distribution: {reasons}")

    swap = {}
    for r in ok:
        sc = r["body"].get("aiplat", {}).get("swap_class") or "none"
        swap[sc] = swap.get(sc, 0) + 1
    findings.append(f"- swap_class distribution: {swap}")

    return findings, {"ok": len(ok), "errors": len(errs)}


def fetch_live_summary(cfg, sess, org=None):
    url = cfg["usage_api_url"] + "/usage/summary?days=1"
    status, body = sess.get(url)
    return status, body


def fetch_alert_history(cfg, sess):
    url = cfg["usage_api_url"] + "/usage/alerts?days=1"
    status, body = sess.get(url)
    return status, body


def fetch_audit_sample(cfg, sess):
    url = cfg["audit_api_url"] + "/audit/records?limit=20"
    status, body = sess.get(url)
    return status, body


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--live-summary", action="store_true")
    args = ap.parse_args()

    cfg = load_config()
    data = load_results()
    findings, counts = analyze_local(data)

    lines = ["# AIPlat load-test report", ""]
    lines.append(f"Run summary: {json.dumps(data['summary'], indent=2)}")
    lines.append("")
    lines.append("## Findings")
    lines.extend(findings)

    if args.live_summary:
        sess = AIPlatSession(cfg)
        sess.login()
        s_status, s_body = fetch_live_summary(cfg, sess)
        a_status, a_body = fetch_alert_history(cfg, sess)
        au_status, au_body = fetch_audit_sample(cfg, sess)
        lines.append("")
        lines.append("## Live /usage/summary (last 24h)")
        lines.append(f"status={s_status}")
        lines.append("```json")
        lines.append(json.dumps(s_body, indent=2)[:4000])
        lines.append("```")
        lines.append("")
        lines.append("## Live /usage/alerts (firings, last 24h)")
        lines.append(f"status={a_status} count={a_body.get('count') if isinstance(a_body, dict) else 'n/a'}")
        lines.append("```json")
        lines.append(json.dumps(a_body, indent=2)[:3000])
        lines.append("```")
        lines.append("")
        lines.append("## Live /audit/records sample")
        lines.append(f"status={au_status} count={au_body.get('count') if isinstance(au_body, dict) else 'n/a'}")
        lines.append("```json")
        lines.append(json.dumps(au_body, indent=2)[:3000])
        lines.append("```")

    report = "\n".join(lines)
    print(report)
    with open(REPORT_PATH, "w") as f:
        f.write(report)
    print(f"\n[done] wrote {REPORT_PATH}")


if __name__ == "__main__":
    main()
