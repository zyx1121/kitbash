# A web application

An `http` Process is served at `https://<name>.<member>.<domain>` with TLS,
and kitbashd is its reverse proxy. The host instructions carry a minimal
Package; this is what they leave out.

## The unit

```yaml
name: app
description: What this does, one sentence.
deploy:
  units:
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
      environment: { NODE_ENV: production }
      secrets: [SESSION_SECRET]
      health: { http: /healthz, interval: 30s }
      limits: { memory: "512Mi" }
```

## Rules the Process has to follow

- Listen on `0.0.0.0` and on exactly the `port` the unit declares; the proxy
  and the health probe reach the container through that port.
- The address is public. Anything that is not meant for everyone needs its own
  login or key check in the application, or a proxy unit in front as
  serve-ollama does.
- Serve a cheap `/healthz` that checks what the application needs, its
  database for example, and answers `503` until it is there. kitbashd records
  the probe; it restarts nothing.
- Read keys and passwords from environment variables named under `secrets`,
  set with `secrets_set`. Never write them into Files or the Dockerfile.

## State

- A cache or a database is a second unit of the same Package, reached on
  `localhost`; see the postgres recipe.
- Files that must survive a restart go through a `mounts` entry to a folder
  inside the Package, with a `.gitignore` that ignores its contents, as the
  postgres recipe shows; without it the files the Process writes leave the
  folder with uncommitted changes and `pkg_build` refuses it. Anything written
  elsewhere in the container is gone once the Process is replaced by a new
  build.

## Build

- kitbash builds from the last commit, not the working tree: write every file
  with `fs_write` before `pkg_build`.
- A build context is a relative path without a leading `./`: `build: .` or
  `build: web`, never `build: ./web`, which `pkg_build` refuses.
- An `image:` unit must be pinned by digest, `name@sha256:...`. A `FROM` line
  in a Dockerfile may use a tag.
- Frameworks that build at image time, Next.js or Vite for example, build in
  the Dockerfile and start with the production server, not the dev server.

Verified on kitbash 0.15.0, 2026-09-23 with a Node server and a Postgres unit,
see the postgres recipe.
