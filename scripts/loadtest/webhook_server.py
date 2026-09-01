#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""Local webhook you can hit to trigger a customizable load-test run.

Not a real "webhook receiver" (AIPlat's alerts.webhook_url still needs a
public HTTPS endpoint reachable from AWS — see the README section on ngrok/
API Gateway for exposing this publicly if you want alert firings delivered
here). This is the trigger side: a small HTTP server you POST to whenever you
want to kick off a test run, with the size/scope you choose per call.

Run:
  python3 webhook_server.py --port 8787

Then, from anywhere on your machine (or exposed via ngrok for remote/CI use):
  curl -X POST http://localhost:8787/run -H 'content-type: application/json' -d '{
    "teams": 10, "apps_per_team": 10, "requests": 200,
    "targets": ["cost", "latency", "cache", "semantic_cache", "guardrails", "alerts"],
    "org": "XYZ_ORG"
  }'

Each run is logged under scripts/loadtest/runs/<timestamp>/ with seed output,
load output, and the analysis report — so repeated webhook triggers never
overwrite each other's evidence.
"""
import argparse
import http.server
import json
import os
import socketserver
import subprocess
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
RUNS_DIR = os.path.join(HERE, "runs")

VALID_TARGETS = {"cost", "latency", "cache", "semantic_cache", "guardrails", "alerts", "all"}


class Handler(http.server.BaseHTTPRequestHandler):
    def _send(self, status, body):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        if self.path != "/run":
            self._send(404, {"error": "unknown path, use POST /run"})
            return
        length = int(self.headers.get("content-length", 0))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            params = json.loads(raw)
        except json.JSONDecodeError:
            self._send(400, {"error": "invalid JSON body"})
            return

        teams = int(params.get("teams", 5))
        apps_per_team = int(params.get("apps_per_team", 5))
        requests_n = int(params.get("requests", 50))
        org = params.get("org")
        targets = params.get("targets", ["all"])
        skip_seed = bool(params.get("skip_seed", False))

        bad = [t for t in targets if t not in VALID_TARGETS]
        if bad:
            self._send(400, {"error": f"unknown targets: {bad}, valid: {sorted(VALID_TARGETS)}"})
            return

        run_id = time.strftime("%Y%m%d-%H%M%S")
        run_dir = os.path.join(RUNS_DIR, run_id)
        os.makedirs(run_dir, exist_ok=True)
        with open(os.path.join(run_dir, "params.json"), "w") as f:
            json.dump(params, f, indent=2)

        # Runs in a background thread so the HTTP call returns immediately;
        # the caller polls run_dir (or checks /status) for completion.
        threading.Thread(
            target=_execute_run,
            args=(run_dir, teams, apps_per_team, requests_n, org, targets, skip_seed),
            daemon=True,
        ).start()

        self._send(202, {"run_id": run_id, "run_dir": run_dir, "status": "started"})

    def do_GET(self):
        if self.path.startswith("/status/"):
            run_id = self.path.split("/status/", 1)[1]
            run_dir = os.path.join(RUNS_DIR, run_id)
            done_marker = os.path.join(run_dir, "DONE")
            if not os.path.isdir(run_dir):
                self._send(404, {"error": "unknown run_id"})
                return
            status = "done" if os.path.exists(done_marker) else "running"
            self._send(200, {"run_id": run_id, "status": status})
            return
        self._send(404, {"error": "GET /status/<run_id>, or POST /run to start"})

    def log_message(self, fmt, *args):
        pass  # keep stdout clean; runs log to their own files


def _execute_run(run_dir, teams, apps_per_team, requests_n, org, targets, skip_seed):
    seed_log = os.path.join(run_dir, "seed.log")
    load_log = os.path.join(run_dir, "load.log")
    report_path = os.path.join(run_dir, "report.md")

    env = os.environ.copy()

    if not skip_seed:
        cmd = ["python3", "-u", "seed.py", "--teams", str(teams), "--apps-per-team", str(apps_per_team)]
        if org:
            cmd += ["--org", org]
        if "alerts" in targets or "all" in targets:
            pass  # webhook_url wiring is left to the operator (see module docstring)
        with open(seed_log, "w") as f:
            subprocess.run(cmd, cwd=HERE, stdout=f, stderr=subprocess.STDOUT, env=env)

    guardrail_every = 15 if ("guardrails" in targets or "all" in targets) else 0
    repeat = 3 if ("cache" in targets or "semantic_cache" in targets or "all" in targets) else 1000000
    load_cmd = [
        "python3", "-u", "load.py",
        "--requests", str(requests_n),
        "--workers", "6",
        "--repeat-prompt", str(repeat),
        "--guardrail-every", str(guardrail_every),
    ]
    with open(load_log, "w") as f:
        subprocess.run(load_cmd, cwd=HERE, stdout=f, stderr=subprocess.STDOUT, env=env)

    # Copy results.json into the run dir before analyze overwrites the shared one.
    results_src = os.path.join(HERE, "results.json")
    if os.path.exists(results_src):
        with open(results_src) as f:
            data = f.read()
        with open(os.path.join(run_dir, "results.json"), "w") as f:
            f.write(data)

    analyze_cmd = ["python3", "-u", "analyze.py", "--live-summary"]
    with open(os.path.join(run_dir, "analyze.log"), "w") as f:
        subprocess.run(analyze_cmd, cwd=HERE, stdout=f, stderr=subprocess.STDOUT, env=env)
    report_src = os.path.join(HERE, "report.md")
    if os.path.exists(report_src):
        with open(report_src) as f:
            data = f.read()
        with open(report_path, "w") as f:
            f.write(data)

    with open(os.path.join(run_dir, "DONE"), "w") as f:
        f.write(time.strftime("%Y-%m-%d %H:%M:%S"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8787)
    args = ap.parse_args()
    os.makedirs(RUNS_DIR, exist_ok=True)

    class ReusableServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
        allow_reuse_address = True

    srv = ReusableServer(("127.0.0.1", args.port), Handler)
    print(f"[webhook] listening on http://127.0.0.1:{args.port}  (POST /run, GET /status/<run_id>)")
    srv.serve_forever()


if __name__ == "__main__":
    main()
