"""Scenario 1 (Anthropic part): the official Anthropic SDK works against the gateway by
changing only base URL and key — chat, streaming, tool use, images — over both an
OpenAI-format upstream and an Anthropic-format upstream."""

import anthropic
import pytest

from conftest import KEY_A

MODELS = ["sima/qwen-9b", "simb-anth/claude-local"]
TOOLS = [{"name": "get_weather", "description": "Weather for a city",
          "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}]


@pytest.fixture(scope="module")
def client(base_url):
    return anthropic.Anthropic(base_url=base_url, api_key=KEY_A, max_retries=0)


# Covers: API-02, API-06, NFR-20
@pytest.mark.parametrize("model", MODELS)
def test_messages(client, model):
    raw = client.messages.with_raw_response.create(
        model=model, max_tokens=200,
        system=[{"type": "text", "text": "Be brief.", "cache_control": {"type": "ephemeral"}}],
        messages=[{"role": "user", "content": "hello gateway"}])
    msg = raw.parse()
    assert msg.content[0].text and msg.stop_reason == "end_turn" and msg.usage.output_tokens > 0
    assert raw.headers["x-gw-served-model"] == model and float(raw.headers["x-gw-cost-usd"]) > 0


# Covers: API-02, NFR-20
@pytest.mark.parametrize("model", MODELS)
def test_streaming(client, model):
    with client.messages.stream(model=model, max_tokens=200, messages=[{"role": "user", "content": "stream about rivers"}]) as s:
        text = "".join(s.text_stream)
        final = s.get_final_message()
    assert "rivers" in text and final.stop_reason == "end_turn"


# Covers: API-02, API-06, NFR-20
@pytest.mark.parametrize("model", MODELS)
def test_tool_use_round_trip(client, model):
    msg = client.messages.create(model=model, max_tokens=200, tools=TOOLS,
                                 messages=[{"role": "user", "content": "weather in Pune?"}])
    call = next(b for b in msg.content if b.type == "tool_use")
    assert msg.stop_reason == "tool_use" and call.name == "get_weather" and "city" in call.input
    msg2 = client.messages.create(model=model, max_tokens=200, tools=TOOLS, messages=[
        {"role": "user", "content": "weather in Pune?"},
        {"role": "assistant", "content": msg.content},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": call.id, "content": "31C and sunny"}]}])
    assert "31C" in msg2.content[0].text


# Covers: API-02, NFR-20
def test_streaming_tool_use(client):
    with client.messages.stream(model="simb-anth/claude-local", max_tokens=200, tools=TOOLS,
                                messages=[{"role": "user", "content": "weather in Goa?"}]) as s:
        final = s.get_final_message()
    call = next(b for b in final.content if b.type == "tool_use")
    assert call.input.get("city")


# Covers: API-02, NFR-20
def test_image_input(client):
    msg = client.messages.create(model="simb-anth/claude-local", max_tokens=50, messages=[{"role": "user", "content": [
        {"type": "text", "text": "what is this?"},
        {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}]}])
    assert msg.content[0].text


# Covers: API-10, NFR-20
def test_errors_map_to_sdk_exceptions(base_url):
    bad = anthropic.Anthropic(base_url=base_url, api_key="wrong", max_retries=0)
    with pytest.raises(anthropic.AuthenticationError):
        bad.messages.create(model="auto", max_tokens=5, messages=[{"role": "user", "content": "x"}])
