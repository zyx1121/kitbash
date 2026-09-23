# Postgres for a Package

Postgres runs as a unit beside the application in the same pod. The
application reaches it at `127.0.0.1:5432`, and nothing outside the pod can:
the proxy routes HTTP by host name, and Postgres speaks its own protocol.

## The units

Add the `db` unit to the application's `kitbash.yaml` and give the
application the connection settings. `pgdata` is a folder inside the Package.

```yaml
deploy:
  units:
    - name: web
      type: container
      build: .
      expose: http
      port: 8080
      environment:
        PGHOST: "127.0.0.1"
        PGUSER: "app"
        PGDATABASE: "app"
      secrets: [POSTGRES_PASSWORD]
      health: { http: /healthz, interval: 30s }
    - name: db
      type: container
      image: docker.io/library/postgres@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24
      environment:
        POSTGRES_USER: "app"
        POSTGRES_DB: "app"
        PGDATA: /var/lib/postgresql/data/pgdata
      secrets: [POSTGRES_PASSWORD]
      mounts:
        - { source: /home/<member>/<package>/pgdata, target: /var/lib/postgresql/data, mode: rw }
```

`pgdata/.gitignore`, written with the rest of the Package:

```
*
!.gitignore
```

## Steps

1. `secrets_set POSTGRES_PASSWORD`. Both units declare the same name, so they
   receive the same value.
2. Write the manifest, the application and `pgdata/.gitignore` in one
   `fs_write`, then `pkg_build` and `proc_run`.
3. The application starts beside the database, not after it: connect in a
   retry loop and let `/healthz` answer `503` until a `SELECT 1` works.

## Traps

- The `.gitignore` is what keeps the database out of Files. Writes through a
  mount are not committed, but the member's next `fs_write` commits everything
  the Process left in the repository. Without the ignore file, the next commit
  takes the whole database with it.
- `PGDATA` points one level below the mount target. The image initializes an
  empty directory only, and the mount root already holds `.gitignore`.
- Postgres writes the files as its own user inside the container, which is a
  subordinate uid on the host. The member cannot read `pgdata/pgdata`
  directly; read data through the application or `psql` in the db unit.
- The password is read when the data directory is created. Rotating
  `POSTGRES_PASSWORD` later needs an `ALTER USER` inside the database as well.
- `limits.memory` on the db unit is the ceiling for `shared_buffers` and work
  memory together; set the Postgres settings below it.

Verified on kitbash 0.15.0, 2026-09-23: a Node counter in the web unit, the
count kept its value across `proc_stop` and `proc_run`, a later `fs_write`
committed only the file it named, and `pkg_build` still ran with a populated
`pgdata`.
