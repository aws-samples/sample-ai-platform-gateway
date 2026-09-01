#!/usr/bin/env python3
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
"""One-off: confirms the SSN masking fix reaches the provider correctly.
Sends a prompt containing a fake SSN and asks the model to echo back
EXACTLY what it received — if mask_pii is working, the model should see
"[ssn]" instead of the digits, and its echo should reflect that.
"""
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
        "messages": [{
            "role": "user",
            # Framed as a word-count task rather than "repeat back my SSN" —
            # avoids triggering Claude's own safety refusal, while still
            # requiring it to quote the exact string it received (proving
            # what actually reached the provider).
            "content": "Count the words in this exact string and then quote the string back verbatim in your answer: 123-45-6789 test string",
        }],
    }
    req = urllib.request.Request(
        url, data=json.dumps(body).encode(), method="POST",
        headers={"content-type": "application/json", "authorization": "Bearer " + api_key},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            data = json.loads(r.read())
    except urllib.error.HTTPError as e:
        print("status:", e.code)
        print(e.read().decode())
        return
    reply = data["choices"][0]["message"]["content"]
    print("model saw / echoed:", repr(reply))
    if "123-45-6789" in reply:
        print("FAIL: raw SSN reached the model and was echoed back — masking did not apply.")
    elif "[ssn]" in reply.lower():
        print("PASS: model quoted back a masked [ssn] placeholder, not the raw SSN.")
    else:
        print("INCONCLUSIVE: model didn't quote the string as asked (LLMs sometimes paraphrase) — check the raw reply above by eye.")


if __name__ == "__main__":
    main()
