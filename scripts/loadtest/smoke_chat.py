#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""One-off: fires a single real chat/completions call against the gateway using
the first key in state.json, to validate the Bedrock route end-to-end before
scaling up the load generator."""
import json
import os
import urllib.error
import urllib.request

from aiplat_client import load_config

STATE_PATH = os.path.join(os.path.dirname(__file__), "state.json")


def main():
    cfg = load_config()
    with open(STATE_PATH) as f:
        state = json.load(f)
    api_key = state["teams"][0]["apps"][0]["api_key"]
    url = cfg["gateway_url"] + "/v1/chat/completions"
    body = {
        "model": cfg["default_bedrock_model"]["alias"],
        "messages": [{"role": "user", "content": "Say the word PONG and nothing else."}],
    }
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        method="POST",
        headers={"content-type": "application/json", "authorization": "Bearer " + api_key},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            print("status:", r.status)
            print(r.read().decode())
    except urllib.error.HTTPError as e:
        print("status:", e.code)
        print(e.read().decode())


if __name__ == "__main__":
    main()
