# Serve a GGUF model with llama.cpp

One Process, one unit, one model: `llama-server` downloads the GGUF named in
`LLAMA_ARG_HF_REPO` at start and serves an OpenAI compatible API at
`https://<name>.<member>.<domain>/v1`. It checks the API key itself, so no
proxy is needed.

## When to use it

- A particular GGUF: a quantization, a fine tune, or a model that only exists
  as GGUF on Hugging Face.
- An architecture llama.cpp supports before Ollama does.
- The smallest image and memory for a model on CPU.
- Choose serve-ollama to switch between models, serve-vllm for many
  concurrent users on a GPU.

## The Package

`kitbash.yaml`, and nothing else: the upstream image is configured entirely
through environment variables.

```yaml
name: gguf
description: llama-server serving Qwen3 0.6B Q8_0, API key required.
deploy:
  units:
    - type: container
      image: ghcr.io/ggml-org/llama.cpp@sha256:fb8f521cdfee1b763a6ef0d6633922e780c1393c03b49945526550cf55010343
      expose: http
      port: 8080
      environment:
        LLAMA_ARG_HF_REPO: "Qwen/Qwen3-0.6B-GGUF:Q8_0"
        LLAMA_ARG_HOST: "0.0.0.0"
        LLAMA_ARG_PORT: "8080"
        LLAMA_ARG_CTX_SIZE: "8192"
      secrets: [LLAMA_API_KEY]
      health: { http: /health, interval: 30s }
      limits: { memory: "2Gi" }
```

## Steps

1. `secrets_set LLAMA_API_KEY` to a long random value.
2. Write the manifest, `pkg_build`, `proc_run`.
3. Wait until `GET /health` answers `{"status":"ok"}`; it answers `503` while
   the model downloads and loads.
4. Check that a request without the key is refused before handing out the
   address: `POST /v1/chat/completions` with no `Authorization` must be `401`.
5. Clients use base URL `https://<name>.<member>.<domain>/v1` and the key. The
   `model` field may be anything; the server has one model.

## Traps

- The key variable is `LLAMA_API_KEY`. Every other option is `LLAMA_ARG_*`,
  and `LLAMA_ARG_API_KEY` is silently ignored: the server then starts with no
  key and answers everyone. Always do step 4.
- `/health` is public by design, so the kitbashd probe works without the key.
  Every other endpoint, including `/v1/models`, requires it.
- `LLAMA_ARG_HF_REPO` is `<repo>:<quant>`, for example `:Q4_K_M` or `:Q8_0`.
  The GGUF is downloaded again at every `proc_run` until kitbash has volumes,
  so prefer small quants for models that restart often.
- `LLAMA_ARG_CTX_SIZE` sets the context and most of the memory: the KV cache
  grows with it. Raise `limits.memory` with it.
- On a host without a GPU the CPU image is the right one. A CUDA image and
  `LLAMA_ARG_N_GPU_LAYERS` wait on `limits.gpu`, PLAN.md 5.6 M13.

Verified on kitbash 0.15.0, 2026-09-23: ready 17 s after start with the
download, about 69 tokens per second for the 0.6B Q8_0 on 4 CPUs, and every
endpoint but `/health` refused without the key once the variable was
`LLAMA_API_KEY`.
