"""Scenario harness: builds and starts the simulated provider and the gateway as real
processes, then hands tests a base URL and demo keys. Scenarios use the official SDKs.

Run: make scenarios   (or: pytest scenarios -q)
Set GATEWAY_URL to run against an already-running gateway instead.
"""

import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

import pytest

ROOT = pathlib.Path(__file__).resolve().parent.parent
BIN = ROOT / "bin"
KEY_A = "gw-demo-a_" + "a" * 32
KEY_B = "gw-demo-b_" + "b" * 32
ADMIN_KEY = "gw-admin_" + "c" * 32


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_http(url, timeout=20):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=1) as r:
                if r.status < 500:
                    return
        except Exception:
            time.sleep(0.1)
    raise RuntimeError(f"{url} did not come up")


def build():
    BIN.mkdir(exist_ok=True)
    for cmd in ("gateway", "simprovider"):
        subprocess.run(["go", "build", "-o", str(BIN / cmd), f"./cmd/{cmd}"], cwd=ROOT, check=True)


class Stack:
    def __init__(self, config=None, extra_env=None):
        self.procs = []
        self.tmp = tempfile.mkdtemp(prefix="gwscen-")
        self.sima_port, self.simb_port, self.gw_port = free_port(), free_port(), free_port()
        self.sima = f"http://127.0.0.1:{self.sima_port}"
        self.simb = f"http://127.0.0.1:{self.simb_port}"
        self.url = f"http://127.0.0.1:{self.gw_port}"
        self.config = config or str(ROOT / "deploy/demo/gateway.yaml")
        self.extra_env = extra_env or {}

    def env(self):
        e = dict(os.environ)
        e.update({
            "GATEWAY_LISTEN": f"127.0.0.1:{self.gw_port}",
            "SIMA_URL": self.sima + "/v1", "SIMB_URL": self.simb + "/v1",
            "DEMO_KEY_A": KEY_A, "DEMO_KEY_B": KEY_B, "DEMO_ADMIN_KEY": ADMIN_KEY,
            "WAL_DIR": os.path.join(self.tmp, "wal"),
        })
        e.update(self.extra_env)
        return e

    def start(self):
        env = self.env()
        oracle = ROOT / "testdata/corpus/corpus.jsonl"
        sim_args = [str(BIN / "simprovider"), "-instance", f"sima=127.0.0.1:{self.sima_port}",
                    "-instance", f"simb=127.0.0.1:{self.simb_port}"]
        if oracle.exists():
            sim_args += ["-oracle", str(oracle)]
        self.procs.append(subprocess.Popen(sim_args, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        wait_http(self.sima + "/v1/models")
        self.log = open(os.path.join(self.tmp, "gateway.log"), "w")
        self.procs.append(subprocess.Popen([str(BIN / "gateway"), "-config", self.config], env=env,
                                           stdout=self.log, stderr=subprocess.STDOUT))
        wait_http(self.url + "/livez")
        return self

    def stop(self):
        for p in reversed(self.procs):
            p.terminate()
            try:
                p.wait(10)
            except subprocess.TimeoutExpired:
                p.kill()
        shutil.rmtree(self.tmp, ignore_errors=True)

    def sim_faults(self, instance, faults, model=""):
        import json
        base = self.sima if instance == "sima" else self.simb
        req = urllib.request.Request(base + "/_sim/faults", data=json.dumps({"model": model, "faults": faults}).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
        urllib.request.urlopen(req).read()


@pytest.fixture(scope="session")
def stack():
    if os.environ.get("GATEWAY_URL"):
        s = Stack()
        s.url = os.environ["GATEWAY_URL"]
        yield s
        return
    build()
    s = Stack().start()
    yield s
    s.stop()


@pytest.fixture(scope="session")
def base_url(stack):
    return stack.url
