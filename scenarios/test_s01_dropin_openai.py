"""Scenario 1 (OpenAI part): the official OpenAI SDK works against the gateway by
changing only base URL and key; response headers show served model, cost and route."""

import json

import openai
import pytest

from conftest import KEY_A

GW_HEADERS = ("x-gw-served-model", "x-gw-cost-usd", "x-gw-route-reason", "x-request-id")


@pytest.fixture(scope="module")
def client(base_url):
    return openai.OpenAI(base_url=base_url + "/v1", api_key=KEY_A, max_retries=0)


# Covers: API-01, API-09, NFR-20
def test_chat(client):
    raw = client.chat.completions.with_raw_response.create(
        model="sima/qwen-9b", messages=[{"role": "system", "content": "be brief"}, {"role": "user", "content": "hello gateway"}])
    for h in GW_HEADERS:
        assert raw.headers.get(h), h
    resp = raw.parse()
    assert resp.choices[0].message.content
    assert resp.usage.prompt_tokens > 0
    assert float(raw.headers["x-gw-cost-usd"]) > 0


# Covers: API-01, API-13, NFR-20
def test_streaming(client):
    stream = client.chat.completions.create(model="sima/qwen-4b", stream=True, stream_options={"include_usage": True},
                                            messages=[{"role": "user", "content": "stream please"}])
    text, usage = "", None
    for chunk in stream:
        if chunk.choices:
            text += chunk.choices[0].delta.content or ""
        if chunk.usage:
            usage = chunk.usage
    assert "stream please" in text
    assert usage and usage.completion_tokens > 0


# Covers: API-01, NFR-20
def test_tool_calls(client):
    tools = [{"type": "function", "function": {"name": "get_weather", "parameters": {
        "type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}]
    resp = client.chat.completions.create(model="sima/qwen-9b", tools=tools,
                                          messages=[{"role": "user", "content": "weather in Pune"}])
    call = resp.choices[0].message.tool_calls[0]
    assert call.function.name == "get_weather"
    assert "city" in json.loads(call.function.arguments)


# Covers: API-01, NFR-20
def test_json_schema(client):
    schema = {"type": "object", "properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
              "required": ["name", "age"], "additionalProperties": False}
    resp = client.chat.completions.create(
        model="sima/qwen-4b", messages=[{"role": "user", "content": "Asha is 34"}],
        response_format={"type": "json_schema", "json_schema": {"name": "person", "schema": schema, "strict": True}})
    obj = json.loads(resp.choices[0].message.content)
    assert set(obj) == {"name", "age"}


# Covers: API-03, NFR-20
def test_embeddings(client):
    raw = client.embeddings.with_raw_response.create(model="sima/nomic-embed", input=["hello", "world"])
    resp = raw.parse()
    assert len(resp.data) == 2 and len(resp.data[0].embedding) == 256
    assert raw.headers.get("x-gw-cost-usd")


# Covers: API-04, NFR-20
def test_models(client):
    ids = {m.id for m in client.models.list()}
    assert {"auto", "sima/qwen-9b", "sima/nomic-embed"} <= ids


# Covers: API-10, NFR-20
def test_errors_surface_as_sdk_exceptions(base_url):
    bad = openai.OpenAI(base_url=base_url + "/v1", api_key="wrong", max_retries=0)
    with pytest.raises(openai.AuthenticationError):
        bad.models.list()
