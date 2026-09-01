# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

"""Shared HTTP + Cognito auth client for the AIPlat load-test harness.

Stdlib only (no `requests`/`boto3` HTTP dependency for the API calls — boto3 is
used only for the Cognito login, since it's already available in this env).
"""
import json
import os
import time
import urllib.error
import urllib.request

import boto3

CONFIG_PATH = os.path.join(os.path.dirname(__file__), "config.json")


def load_config():
    with open(CONFIG_PATH) as f:
        return json.load(f)


def load_env():
    """Reads username/password from scripts/loadtest/.env (KEY=VALUE, no quoting)."""
    env_path = os.path.join(os.path.dirname(__file__), ".env")
    out = {}
    if os.path.exists(env_path):
        with open(env_path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip()
    for k in ("AIPLAT_USER", "AIPLAT_PASS"):
        if k in os.environ:
            out[k] = os.environ[k]
    return out


class AIPlatSession:
    """Logs in once via Cognito USER_PASSWORD_AUTH and reuses the JWT id_token
    (Bearer) for every domain API (governance/core/observability/audit) —
    they all share the same Cognito user pool + client, per the Terraform
    contract (each domain reads the pool via SSM)."""

    def __init__(self, cfg=None):
        self.cfg = cfg or load_config()
        env = load_env()
        self.username = env.get("AIPLAT_USER")
        self.password = env.get("AIPLAT_PASS")
        if not self.username or not self.password:
            raise SystemExit(
                "Missing AIPLAT_USER/AIPLAT_PASS. Copy scripts/loadtest/.env.example "
                "to scripts/loadtest/.env and fill in credentials."
            )
        self._token = None
        self._claims = None
        self._cip = boto3.client("cognito-idp", region_name=self.cfg["region"])

    def login(self):
        resp = self._cip.initiate_auth(
            ClientId=self.cfg["cognito_client_id"],
            AuthFlow="USER_PASSWORD_AUTH",
            AuthParameters={"USERNAME": self.username, "PASSWORD": self.password},
        )
        result = resp.get("AuthenticationResult")
        if not result:
            raise SystemExit(f"Cognito login returned a challenge, not tokens: {resp}")
        self._token = result["IdToken"]
        self._claims = _decode_jwt_claims(self._token)
        return self._claims

    @property
    def token(self):
        if not self._token:
            self.login()
        return self._token

    @property
    def claims(self):
        if not self._claims:
            self.login()
        return self._claims

    def request(self, method, url, body=None, headers=None, retries=3):
        hdrs = {"content-type": "application/json", "authorization": "Bearer " + self.token}
        if headers:
            hdrs.update(headers)
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        last_err = None
        for attempt in range(retries):
            try:
                with urllib.request.urlopen(req, timeout=60) as r:
                    raw = r.read()
                    status = r.status
                    return status, _safe_json(raw)
            except urllib.error.HTTPError as e:
                raw = e.read()
                return e.code, _safe_json(raw)
            except (urllib.error.URLError, TimeoutError) as e:
                last_err = e
                time.sleep(1.5 * (attempt + 1))
        raise RuntimeError(f"request failed after {retries} retries: {last_err}")

    def get(self, url, headers=None):
        return self.request("GET", url, None, headers)

    def post(self, url, body, headers=None):
        return self.request("POST", url, body, headers)

    def put(self, url, body, headers=None):
        return self.request("PUT", url, body, headers)

    def delete(self, url, headers=None):
        return self.request("DELETE", url, None, headers)


def _safe_json(raw):
    if not raw:
        return {}
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return {"_raw": raw.decode(errors="replace")}


def _decode_jwt_claims(jwt):
    import base64

    parts = jwt.split(".")
    if len(parts) != 3:
        return {}
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    return json.loads(base64.urlsafe_b64decode(payload))
