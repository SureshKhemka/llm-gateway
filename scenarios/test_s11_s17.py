"""Scenario 11 (data residency and PII) and scenario 17 (Claude Code through the
gateway) against real processes."""

import json
import os
import shutil
import subprocess
import tempfile
import time
import urllib.request

import openai
import pytest

from conftest import ADMIN_KEY, KEY_A

HARD = "Reason step by step: prove the bound, then derive the corollary and analyze the trade-offs."


def explain(base_url, headers=None):
    req = urllib.request.Request(base_url + "/v1/route/explain", method="POST",
                                 data=json.dumps({"model": "auto", "messages": [{"role": "user", "content": HARD}]}).encode(),
                                 headers={"Authorization": "Bearer " + KEY_A, "Content-Type": "application/json", **(headers or {})})
    return json.loads(urllib.request.urlopen(req).read())["decision"]


# Covers: GRD-02, GRD-04
def test_s11_residency_and_pii(stack, base_url):
    normal = explain(base_url)
    assert any(c["deployment"] == "cloud/frontier" for c in normal["candidates"])
    conf = explain(base_url, {"X-Gw-Sensitivity": "confidential"})
    assert all(c["deployment"] != "cloud/frontier" for c in conf["candidates"])
    assert any(e["deployment"] == "cloud/frontier" and "residency" in e["reasons"] for e in conf["exclusions"])
    client = openai.OpenAI(base_url=base_url + "/v1", api_key=KEY_A, max_retries=0)
    client.chat.completions.create(model="auto", messages=[{"role": "user", "content": "Mail asha.k@example.in, PAN ABCPE1234F"}])
    time.sleep(0.3)
    stack.log.flush()
    lines = [json.loads(l) for l in open(stack.log.name) if l.startswith('{"type":"request"')]
    assert lines[-1]["pii_detected"] == ["email", "pan"]
    assert "asha.k@example.in" not in open(stack.log.name).read()


# Covers: API-02, NFR-20
def test_s17_claude_code(stack, base_url):
    claude = shutil.which("claude")
    if not claude:
        pytest.skip("Claude Code CLI not installed")
    home = tempfile.mkdtemp(prefix="cc-home-")
    # Claude Code forwards ANTHROPIC_CUSTOM_HEADERS: the confidential label keeps the
    # session on local deployments even though a cheaper cloud model is configured.
    env = {"PATH": os.environ["PATH"], "HOME": home, "ANTHROPIC_BASE_URL": base_url, "ANTHROPIC_API_KEY": KEY_A,
           "ANTHROPIC_CUSTOM_HEADERS": "X-Gw-Sensitivity: confidential",
           "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1"}
    out = subprocess.run([claude, "-p", "Reply with the word gateway.", "--model", "claude-sonnet-4-5", "--max-turns", "2"],
                         env=env, capture_output=True, text=True, timeout=180, cwd=home)
    shutil.rmtree(home, ignore_errors=True)
    assert out.returncode == 0, out.stderr[-2000:]
    assert out.stdout.strip(), "Claude Code printed nothing"
    # Every request ran on a local deployment and is attributed to Claude Code's key.
    req = urllib.request.Request(base_url + "/admin/v1/requests?limit=20", headers={"Authorization": "Bearer " + ADMIN_KEY})
    recs = [r for r in json.loads(urllib.request.urlopen(req).read())["requests"] if r["endpoint"] == "messages"]
    assert recs and all(r["status"] == 200 and r["key_id"] == "demo-key-a" for r in recs)
    assert all(r["deployment"] != "cloud/frontier" for r in recs), [r["deployment"] for r in recs]
    print("Claude Code session:", [(r["deployment"], r["input_tokens"], r["cost_usd"]) for r in recs])
