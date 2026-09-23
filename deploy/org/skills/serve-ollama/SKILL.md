# Serve models with Ollama

One Process serves every model in `MODELS` through an OpenAI compatible API at
`https://<name>.<member>.<domain>/v1`, and only to requests that carry the
member's key.

## When to use it

- One person, or a few, who want a model running in minutes.
- Switching between models: one Ollama Process loads whichever model a request
  names, one at a time, so it is one Process for many models.
- Choose serve-vllm instead for many concurrent users on a GPU, and
  serve-llamacpp for a specific GGUF file or an architecture Ollama does not
  ship.

## The Package

Two units in one pod. Ollama has no authentication and an `http` Process is
reachable from the internet, so Ollama listens on the pod's loopback only and
Caddy in front of it checks the key.

`kitbash.yaml`:

```yaml
name: chat
description: Ollama serving qwen3:0.6b behind a bearer token.
deploy:
  units:
    - name: api
      type: container
      build: proxy
      expose: http
      port: 8080
      secrets: [API_KEY]
      health: { http: /healthz, interval: 30s }
    - name: ollama
      type: container
      build: ollama
      environment:
        OLLAMA_HOST: "127.0.0.1:11434"
        MODELS: "qwen3:0.6b"
      limits: { memory: "3Gi" }
```

`proxy/Dockerfile`:

```dockerfile
FROM docker.io/library/caddy@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d
COPY Caddyfile /etc/caddy/Caddyfile
```

`proxy/Caddyfile`:

```
{
	admin off
	auto_https off
}

:8080 {
	handle /healthz {
		rewrite * /api/version
		reverse_proxy 127.0.0.1:11434 {
			header_up Host {upstream_hostport}
		}
	}

	@authorized header Authorization "Bearer {$API_KEY}"
	handle @authorized {
		reverse_proxy 127.0.0.1:11434 {
			header_up Host {upstream_hostport}
			header_up -Origin
			flush_interval -1
		}
	}

	respond "missing or wrong bearer token" 401
}
```

`ollama/Dockerfile`:

```dockerfile
FROM docker.io/ollama/ollama@sha256:57d60e686821ea81a7748a3ec8141308c8b8f95b27105713954abf7a6529e700
COPY start.sh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
```

`ollama/start.sh`:

```sh
#!/bin/sh
set -e
ollama serve &
server=$!
until ollama list >/dev/null 2>&1; do sleep 1; done
for m in $MODELS; do ollama pull "$m"; done
wait "$server"
```

## Steps

1. `secrets_set API_KEY` to a long random value, and give the member that value.
2. Write the five files in one `fs_write`, then `pkg_build` and `proc_run`.
3. Wait until `GET /api/tags` with the key lists the model: the pull runs after
   start, so `running` does not mean the model is there yet.
4. Clients use base URL `https://<name>.<member>.<domain>/v1`, the key as the
   API key, and the Ollama model name, for example `qwen3:0.6b`.

## Traps

- `build: ./proxy` is refused at `pkg_build`; write `build: proxy`.
- Ollama bound to loopback answers `403` to any Host that is not its own, so
  the proxy must send `header_up Host {upstream_hostport}`. Without it every
  request fails while the health probe from kitbashd still passes.
- Weights live in the container until kitbash has volumes. Every `proc_run`
  pulls `MODELS` again, so keep the list to what is needed. Do not mount a
  Files folder for `/root/.ollama`: gigabytes of weights would end up in a
  commit.
- The first request after a model loads is slow while it warms up; measure
  speed from the second.
- On a host without a GPU Ollama runs on the CPU. A 0.6B model answers in a
  few seconds; 7B and larger need more memory than `limits.memory` above and
  are slow. GPU placement waits on `limits.gpu`, PLAN.md 5.6 M13.
- Ollama keeps a model loaded for 5 minutes after the last request. Set
  `OLLAMA_KEEP_ALIVE` in `environment` (`-1` keeps it loaded) when latency
  matters more than memory.

Verified on kitbash 0.15.0, 2026-09-23: no key answers 401, `/healthz` answers
without one, the model was listed 5 s after start, and the second chat
request took 2 s on 4 CPUs.
