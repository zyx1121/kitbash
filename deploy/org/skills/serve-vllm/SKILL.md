# Serve a model with vLLM

> Status: waits on `limits.gpu` and `volumes`, PLAN.md 5.6 M13. A manifest
> that declares `limits.gpu` is refused by this host's schema today. Use
> serve-ollama or serve-llamacpp here, and come back to this recipe on a GPU
> host.

## When to use it

- Several people or agents calling the same model at once: vLLM batches
  requests, where Ollama and llama.cpp mostly serve one at a time.
- A model published as safetensors on Hugging Face, not GGUF.
- Not for trying many models in turn: every model is a Process of its own and
  a start takes a minute or more.

## The Package, once M13 lands

One unit, the upstream image, an API key and three volumes. The field names
`limits.gpu` and `volumes` are the ones M13 specifies.

```yaml
name: qwen
description: vLLM serving Qwen/Qwen3-0.6B on one GPU, API key required.
deploy:
  units:
    - type: container
      image: docker.io/vllm/vllm-openai@sha256:8a69ffad015f138d7170c4ddc429e230a3bc1c1719f67e14324749df200a4b90
      expose: http
      port: 8000
      command:
        - --model=Qwen/Qwen3-0.6B
        - --max-model-len=8192
        - --gpu-memory-utilization=0.8
      secrets: [VLLM_API_KEY, HF_TOKEN]
      health: { http: /health, interval: 30s }
      limits: { gpu: 1, memory: "16Gi" }
      volumes:
        - { name: hf-cache, target: /root/.cache/huggingface }
        - { name: vllm-cache, target: /root/.cache/vllm }
```

`VLLM_API_KEY` is the variable vLLM reads for `--api-key`. `HF_TOKEN` is
needed only for gated models; drop it from `secrets` otherwise.

## What to expect

Measured with the same image, v0.30.0, on an RTX 3080 with 10 GB, 2026-09-23:

- The image is 30.7 GB unpacked. The first pull is the slowest step of the
  whole setup; a host needs the space before the first `pkg_build`.
- Start to ready: 81 s cold, 40 s once `vllm-cache` holds the compiled
  kernels. Without that volume every start compiles again for about 22 s.
- `proc_run` answers `running` long before the model can answer. Wait for
  `/health` to be `200`, then send one request before measuring anything.

## Traps

- vLLM takes 92% of GPU memory by default and refuses to start when less is
  free. Any other process on the card, a desktop included, makes the default
  fail with "Free memory on device ... is less than desired GPU memory
  utilization". Set `--gpu-memory-utilization` below what is actually free.
- `--max-model-len` bounds the KV cache. Without it vLLM sizes for the model's
  full context and may not fit.
- One GPU holds one vLLM Process. A second GPU unit on the same host is
  refused with `conflict` naming the first; stop it before starting another.
- Weights belong in the `hf-cache` volume, never in a mount of Files.
- vLLM's multiprocess engine may need more shared memory than a container
  gets by default; the M13 work has to confirm whether the unit needs a
  larger `/dev/shm`.
