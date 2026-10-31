"""Scenario 13 against real processes: run the eval suite across all deployments,
accept the recommended thresholds, see a worse policy blocked by the promotion gate,
and roll back — all through the admin API."""

import json
import pathlib
import urllib.error
import urllib.request

from conftest import ADMIN_KEY, ROOT


def admin(base_url, method, path, body=None):
    req = urllib.request.Request(base_url + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + ADMIN_KEY, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def run(base_url, targets, **kw):
    st, out = admin(base_url, "POST", "/admin/v1/evals/runs", {"suite": "ref", "tenant": "team-a", "targets": targets, "wait": True,
                                                               "concurrency": 16, **kw})
    assert st == 200 and out["status"] == "done", out
    return out


def quality(out, label):
    return next(t for t in out["report"]["targets"] if t["target"] == label)


# Covers: EVL-02, EVL-05, EVL-07, EVL-08, CFG-06
def test_s13_eval_driven_tuning(base_url):
    lines = (ROOT / "testdata/corpus/corpus.jsonl").read_text().splitlines()
    items = [json.loads(l) for i, l in enumerate(lines) if i % 5 == 0 and '"long_context"' not in l]
    st, _ = admin(base_url, "POST", "/admin/v1/evals/suites", {"id": "ref", "name": "reference", "items": items})
    assert st == 201
    out = run(base_url, [{"kind": "policy", "name": "default"}, {"kind": "policy", "name": "baseline-strongest"}],
              all_deployments=True, baseline="policy:baseline-strongest@1")
    assert any(p["frontier"] for p in out["report"]["pareto"])
    st, rec = admin(base_url, "POST", f"/admin/v1/evals/runs/{out['id']}/recommend", {"policy": "default"})
    assert st == 200 and rec["predicted_cost_usd"] <= rec["current_predicted_cost_usd"]
    st, v2 = admin(base_url, "POST", "/admin/v1/policies", {"name": "default", "objective": "min_cost",
                                                            "difficulty": {"thresholds": rec["recommended_thresholds"]}})
    assert st == 201
    out2 = run(base_url, [{"kind": "policy", "name": "default", "version": 1}, {"kind": "policy", "name": "default", "version": v2["version"]}])
    st, act = admin(base_url, "POST", "/admin/v1/policies/default/activate", {"version": v2["version"], "eval_run": out2["id"]})
    assert st == 200, act
    st, v3 = admin(base_url, "POST", "/admin/v1/policies", {"name": "default", "objective": "min_cost", "difficulty": {"thresholds": [0.97, 0.98, 0.99]}})
    out3 = run(base_url, [{"kind": "policy", "name": "default", "version": v2["version"]}, {"kind": "policy", "name": "default", "version": v3["version"]}])
    st, blocked = admin(base_url, "POST", "/admin/v1/policies/default/activate", {"version": v3["version"], "eval_run": out3["id"]})
    assert st == 409, blocked
    st, rb = admin(base_url, "POST", "/admin/v1/policies/default/rollback", {"version": 1, "reason": "drill"})
    assert st == 200 and rb["active"] == "default@1"
    print("v1 %.1f / v2 %.1f / v3 %.1f; recommended %s" % (
        quality(out2, "policy:default@1")["quality"], quality(out2, f"policy:default@{v2['version']}")["quality"],
        quality(out3, f"policy:default@{v3['version']}")["quality"], rec["recommended_thresholds"]))
