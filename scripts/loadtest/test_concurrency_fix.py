#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Regression check for the org-tree concurrency bug (item #4 in
.kiro/steering/aiplat-loadtest-findings.md). Creates ONE team, then fires
N concurrent POST /admin/apps + POST /admin/keys pairs against it (the exact
pattern that used to corrupt the shared TEAMS#<org> item). Before the fix this
reproduced "app does not exist" within the first couple of teams under an
8-way pool; this script uses a tighter loop (same team, higher concurrency) to
make the race window as likely as possible.

Usage: python3 test_concurrency_fix.py --apps 20 --workers 8
"""
import argparse
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

from aiplat_client import AIPlatSession, load_config
from seed import create_app, create_team, issue_key, rand_suffix


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apps", type=int, default=20)
    ap.add_argument("--workers", type=int, default=8)
    args = ap.parse_args()

    cfg = load_config()
    sess = AIPlatSession(cfg)
    claims = sess.login()
    org = cfg["org"]
    print(f"[auth] logged in as {claims.get('email')}")

    status, body = create_team(sess, cfg, org, f"ConcurrencyTest {rand_suffix()}")
    if status >= 300:
        raise SystemExit(f"team create failed: {status} {body}")
    team_id = body["id"]
    print(f"[team] created {team_id}")

    lock = threading.Lock()
    created = []
    failures = []

    def make_app_and_key(ai):
        app_name = f"ConcApp {ai:03d} {rand_suffix()}"
        s1, b1 = create_app(sess, cfg, org, team_id, app_name)
        if s1 >= 300:
            with lock:
                failures.append(("create", ai, s1, b1))
            return
        app_id = b1["id"]
        # Small delay mirrors real-world timing (a human wouldn't issue the
        # key in the same millisecond as the create) while still landing well
        # within the window where the old bug reproduced.
        time.sleep(0.05)
        s2, b2 = issue_key(sess, cfg, org, team_id, app_id)
        if s2 >= 300:
            with lock:
                failures.append(("key", ai, s2, b2))
            return
        with lock:
            created.append(app_id)

    t0 = time.time()
    with ThreadPoolExecutor(max_workers=args.workers) as pool:
        futs = [pool.submit(make_app_and_key, i) for i in range(args.apps)]
        for f in as_completed(futs):
            f.result()
    elapsed = time.time() - t0

    print(f"\n[result] {len(created)}/{args.apps} app+key pairs succeeded in {elapsed:.1f}s")
    if failures:
        print(f"[result] {len(failures)} FAILURES (this is the bug reproducing if any say 'app does not exist'):")
        for kind, i, status, body in failures:
            print(f"  - #{i} ({kind}): {status} {body}")
        raise SystemExit(1)
    else:
        print("[result] NO FAILURES — concurrency fix holds under this load.")


if __name__ == "__main__":
    main()
