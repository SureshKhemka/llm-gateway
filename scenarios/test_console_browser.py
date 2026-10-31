"""The console works in a real browser against the running gateway (every view, the
request explorer, a Pareto chart and the playground), with no JS errors or CSP
violations, and markup in stored data renders as text."""

import os
import shutil
import subprocess

import httpx
import openai
import pytest

from conftest import ADMIN_KEY, KEY_A, ROOT


XSS_TENANT = '<img src=x onerror="window.__xss=1">'


# Covers: ADM-02, ADM-03, NFR-13
def test_console_in_browser(stack, base_url):
    if not shutil.which("node"):
        pytest.skip("node not installed")
    client = openai.OpenAI(base_url=base_url + "/v1", api_key=KEY_A, max_retries=0)
    for p in ("hello", "Extract the invoice number from INV-2041", "Explain step by step why the sky is blue"):
        client.chat.completions.create(model="auto", messages=[{"role": "user", "content": p}])
    # Markup in data must render as text (XSS check in the browser script).
    r = httpx.post(base_url + "/admin/v1/tenants", headers={"Authorization": "Bearer " + ADMIN_KEY},
                   json={"id": XSS_TENANT, "name": "xss probe"})
    assert r.status_code in (200, 201), r.text
    out = os.environ.get("CONSOLE_SCREENSHOTS", "")
    if out:
        os.makedirs(out, exist_ok=True)
    env = dict(os.environ, NODE_PATH=subprocess.run(["npm", "root", "-g"], capture_output=True, text=True).stdout.strip())
    r = subprocess.run(["node", str(ROOT / "scenarios/console_browser.js"), base_url, ADMIN_KEY, out], capture_output=True, text=True, timeout=180, env=env)
    assert r.returncode == 0, r.stdout + r.stderr
