# LM Studio settings for the demo

The demo routes between several local models served by LM Studio (0.4 or later) on the
host, through its OpenAI-compatible (`/v1/chat/completions`, `/v1/embeddings`,
`/v1/models`) and Anthropic-compatible (`/v1/messages`) endpoints. LM Studio's defaults
are tuned for one person chatting with one model; a gateway needs several models
resident at once. Set the following before `LMSTUDIO=1 deploy/demo/up.sh` [ENV-02].

## 1. Turn off JIT auto-evict

By default LM Studio loads a model on first request ("JIT loading") and **keeps only one
JIT-loaded model in memory**, evicting the previous one. A router alternating between a
tier-1 and a tier-3 model then reloads a multi-GB model on almost every request (cold
loads take 10–60 s and look like timeouts).

*Developer → Server Settings:* turn **off** "Auto-Evict for JIT loaded models" (and keep
"Just-in-Time model loading" on only as a safety net). With auto-evict off, JIT-loaded
models stay until their idle TTL expires, so also raise or disable the idle TTL.

## 2. Preload the models

Load every model the gateway routes to before starting the demo, so none is loaded on
the request path:

```sh
lms server start --port 1234
lms load google/gemma-4-31b              --context-length 16384 --gpu max --identifier google/gemma-4-31b
lms load qwen/qwen3.8-27b                --context-length 16384 --gpu max --identifier qwen/qwen3.8-27b
lms load gemma-4-26b-a4b-it-mlx          --context-length 16384 --gpu max --identifier gemma-4-26b-a4b-it-mlx
lms load mistralai/devstral-small-2-2512 --context-length 32768 --gpu max --identifier mistralai/devstral-small-2-2512
lms load text-embedding-nomic-embed-text-v1.5 --identifier text-embedding-nomic-embed-text-v1.5
lms ps     # everything listed as loaded
```

Keep `--identifier` equal to the `model` field in `deploy/demo/overlays/lmstudio.yaml`.
The gateway probes `/v1/models` every `probe_every` (15 s): a model that disappears is
marked **cold** and routed around rather than triggering a load, and the console shows
it. With `discover: true`, newly loaded models appear in the console catalog for
approval (tier, prices, capabilities).

## 3. Max concurrent predictions per model

LM Studio serves a limited number of parallel predictions per model (*model load
settings → Max Concurrent Predictions*; requests beyond it queue inside LM Studio
where the gateway can't see them). Set it per model, and set the matching deployment's
`concurrency` in the overlay to the **same value**, so the gateway queues (by priority,
for at most `queue_wait`) and falls back to another deployment instead of piling
requests onto one model:

| Model | Max concurrent predictions | Gateway `concurrency` |
|---|---|---|
| 26–31B models | 2 | 2 |
| devstral-small (code) | 2 | 2 |
| small tier 0–1 models (when added) | 4 | 4 |
| embeddings | 4 | (not limited) |

More parallel slots raise throughput but each slot reserves KV cache, so they cost
memory (next section).

## 4. Context lengths that fit

Memory per model ≈ weights + KV cache × context length × parallel slots. On the 48 GB
MacBook Pro, macOS lets the GPU wire roughly 70–75% of RAM by default (about 36 GB), so
not every model in the inventory fits at once at long contexts. Approximate sizes at
4-bit quantisation:

| Model | Weights | Role in the demo |
|---|---|---|
| gemma-4-31b | ~18 GB | tier 3 |
| qwen3.8-27b / qwen3.6-27b | ~16 GB each | tier 3 |
| gemma-4-26b-a4b (MoE, 4B active) | ~15 GB | tier 2 (fast) |
| devstral-small-2 | ~14 GB | tier 2, code |
| whiterabbitneo-13b | ~8 GB | not routed by default |
| nomic-embed-text-v1.5 | <1 GB | embeddings (semantic cache, evals) |

Guidance:

* Keep context lengths to what the demo uses: **16k** for chat models, **32k** for the
  code model, and set each deployment's `context_window` to the loaded value (the router
  excludes deployments whose window is too small instead of letting LM Studio truncate).
* Load **one tier-3 model** at a time for the demo (e.g. qwen3.8-27b), plus gemma-4-26b-a4b,
  devstral and the embedder: ≈ 46 GB of weights is too much; ≈ 30 GB plus KV cache fits.
  Unload the second 27–31B model or the 13B model you are not demonstrating.
* If you must keep more resident, raise the wired limit (`sudo sysctl
  iogpu.wired_limit_mb=40960`; resets at reboot) and leave headroom for macOS.
* Prefer MLX builds on Apple silicon; they load faster and use unified memory well.

## 5. Missing roles

The inventory covers tier 3, code and embeddings. Scenarios 2, 3 and 12 also need
**tier 0–2 small models** and a **vision model**; load, for example, a 1–4B instruct model
and an 8B vision-language model, add deployments for them (copy an entry in the overlay,
set `tier` and `capabilities: [chat, vision]` for the VL model), and the LM Studio suite
(`go test -tags lmstudio ./...`) stops skipping those scenarios. Until then it skips
them with a message naming the missing role.

## 6. From containers

Inside Docker the host is `host.docker.internal`; the compose file sets
`LMSTUDIO_BASE_URL=http://host.docker.internal:1234/v1`. In LM Studio enable
*Serve on Local Network* so the server listens beyond `127.0.0.1`, or Docker Desktop's
host networking can't reach it. Without Docker (`DEMO_NO_DOCKER=1`) the default
`http://localhost:1234/v1` is used.
