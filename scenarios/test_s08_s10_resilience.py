"""Scenarios 8–10 against real processes: provider failover with circuit breaking and
half-open recovery, latency-aware routing, and a mid-stream provider failure."""

import concurrent.futures
import time

import openai
import pytest

from conftest import KEY_A

PROMPT = "Extract the invoice number and total from: Invoice INV-{} for Kaveri Foods, total due ₹12,400."


@pytest.fixture(scope="module")
def client(base_url):
    return openai.OpenAI(base_url=base_url + "/v1", api_key=KEY_A, max_retries=0)


def burst(client, n):
    def one(i):
        raw = client.chat.completions.with_raw_response.create(
            model="auto", messages=[{"role": "user", "content": PROMPT.format(i)}])
        return raw.headers["x-gw-served-model"]
    with concurrent.futures.ThreadPoolExecutor(8) as ex:
        return list(ex.map(one, range(n)))  # raises on any client-visible error


# Covers: RES-01, RES-02, RES-03
def test_s08_provider_failover(stack, client):
    try:
        assert "sima/qwen-4b" in burst(client, 20)
        stack.sim("sima", "DELETE", "/_sim/models/qwen-4b")       # unload the cheap model
        served = burst(client, 30)
        assert "sima/qwen-4b" not in served[-10:]
        stack.kill_sim("simb")                                     # kill a whole provider
        served = burst(client, 30)
        assert all(not s.startswith("simb/") for s in served[-10:])
        # Recovery: reload the model and restart the provider; after the cool-down a
        # half-open probe closes the circuit and traffic returns to the cheapest model.
        stack.sim("sima", "POST", "/_sim/models", {"id": "qwen-4b", "tier": 1})
        stack.start_sim("simb")
        time.sleep(3.5)
        burst(client, 5)
        assert "sima/qwen-4b" in burst(client, 20)
    finally:
        if "simb" not in stack.sims:
            stack.start_sim("simb")


# Covers: RTE-07
def test_s09_latency_aware_routing(stack, client):
    def served():
        raw = client.chat.completions.with_raw_response.create(model="latency", messages=[{"role": "user", "content": "hello"}])
        return raw.headers["x-gw-served-model"]
    try:
        preferred = served()
        assert all(served() == preferred for _ in range(3))
        slow, other = ("sima", "simb/qwen-4b") if preferred.startswith("sima/") else ("simb", "sima/qwen-4b")
        stack.sim_faults(slow, {"latency_ms": 3000}, model="qwen-4b")
        start = time.time()
        while served() != other:
            assert time.time() - start < 60, "traffic did not move within one minute"
        moved = time.time() - start
        assert all(served() == other for _ in range(5))
        print(f"traffic moved after {moved:.1f}s")
    finally:
        stack.clear_faults()


# Covers: RES-04, API-11
def test_s10_mid_stream_failure(stack, client):
    prompt = "Tell me a long story about the monsoon, please."
    story = " ".join(["rain falls on the hills and rivers run"] * 15)
    stack.sim("sima", "POST", "/_sim/oracle", {prompt: {"reference": story, "difficulty": 0}})
    stack.sim_faults("sima", {"drop_after_tokens": 50}, model="qwen-9b")
    try:
        text = ""
        with pytest.raises(openai.APIError) as exc:
            for chunk in client.chat.completions.create(model="sima/qwen-9b", stream=True,
                                                        messages=[{"role": "user", "content": prompt}]):
                if chunk.choices:
                    text += chunk.choices[0].delta.content or ""
        assert "upstream stream failed" in str(exc.value)
        assert len(text.split()) == 50
    finally:
        stack.clear_faults()
