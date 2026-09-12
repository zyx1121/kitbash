# Store fixtures

One SQLite file per released tag that had a store, as that version of kitbashd
wrote it. `upgrade_test.go` copies each one to a temporary directory, opens it
with the current `store.Open`, which is the migration, and reads back what the
old release wrote. A fixture is never opened in place: opening it migrates it.

Each file was produced by a small program built against that tag's
`internal/store`, kept under `gen/<tag>/`. The programs are not built by the
current tree: Go ignores everything under a `testdata` directory, which is why
they can name a package shape that no longer exists.

## What each fixture holds

Every fixture holds the same records, minus what its era had no column for. The
records sit around 2026-01-15T12:00:00Z: three spans at three, two and one hour
before it, and the same for logs and metrics. Retention is set to 720h traces,
168h logs, 90d metrics, which is not the default a fresh store answers with.

| Tag | Size | Spans | Logs | Metrics | Processes | Approvals | Era |
| --- | --- | --- | --- | --- | --- | --- | --- |
| v0.3.0 | 60 KiB | 3 | 3 | 3 | 0 | 0 | Telemetry and retention only |
| v0.4.0 | 88 KiB | 3 | 3 | 3 | 2 | 0 | adds producer and the processes table |
| v0.5.0 | 112 KiB | 3 | 3 | 3 | 2 | 3 | adds container, digest and approvals |
| v0.6.0 | 124 KiB | 3 | 3 | 3 | 2 | 3 | adds caller |
| v0.6.2 | 124 KiB | 3 | 3 | 3 | 2 | 3 | adds the fan out secret |

No fixture carries `internal` on a signal or `permits` on a Process: both
arrive after v0.6.2, so every fixture is opened with the migration adding them.

The two Processes are `01930000-...-0000000000a1`, owned by loki, an admin,
subscribed to Telemetry, and `01930000-...-0000000000a2`, owned by kilo. Their
tokens are the fixed strings `fixture-token-alpha` and `fixture-token-beta`,
hashed the way a real registration hashes one, so the fixture is the same file
every time it is built and a test can look a Process up by token. They are
fixtures: no such token was ever minted for a running Process.

The three approvals are `...b1` pending, `...b2` approved by loki with a note
and a result, and `...b3` rejected by loki with a reason.

## Adding a fixture for a new release

After tagging v0.7.0, from a clone of this repository:

```sh
git worktree add /tmp/kitbash-v0.7.0 v0.7.0
mkdir -p /tmp/kitbash-v0.7.0/cmd/genfixture
cp internal/store/testdata/gen/v0.6.2/main.go /tmp/kitbash-v0.7.0/cmd/genfixture/main.go
# Adjust the copy for anything the new release added to the store, then keep it
# under internal/store/testdata/gen/v0.7.0/main.go as the record of this run.
(cd /tmp/kitbash-v0.7.0 && go run ./cmd/genfixture /tmp/kitbashd-v0.7.0.db)
cp /tmp/kitbashd-v0.7.0.db internal/store/testdata/kitbashd-v0.7.0.db
chmod 0644 internal/store/testdata/kitbashd-v0.7.0.db
git worktree remove --force /tmp/kitbash-v0.7.0
```

Then add the tag to the `fixtures` table in `internal/store/upgrade_test.go`
and a row to the table above, and run `go test ./internal/store/...`.

Three things the program must keep doing:

1. Close the store rather than exiting, so the write ahead log is checkpointed
   and the fixture is one file. There must be no `-wal` or `-shm` beside it.
2. Use fixed timestamps and fixed token strings, so rebuilding a fixture does
   not rewrite every byte of it.
3. Stay small. A fixture is committed, and every one so far is under 200 KiB.

The tag's `internal/store` is what the program is built against, so a call the
new release changed is a compile error in the copy and not a fixture that
quietly holds something else.
